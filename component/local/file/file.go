package file

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/go-kit/log/level"
	"github.com/grafana/agent/component"
	"github.com/grafana/agent/pkg/flow/hcltypes"
	"github.com/hashicorp/hcl/v2"
	"github.com/rfratto/gohcl"
)

// waitReadPeriod holds the time to wait before reading a file while the
// local.file component is running.
//
// This prevents local.file from updating too frequently and exporting partial
// writes.
const waitReadPeriod time.Duration = 30 * time.Millisecond

func init() {
	component.Register(component.Registration{
		Name:    "local.file",
		Args:    Arguments{},
		Exports: Exports{},

		Build: func(opts component.Options, args component.Arguments) (component.Component, error) {
			return New(opts, args.(Arguments))
		},
	})
}

// Arguments holds values which are used to configure the local.file component.
type Arguments struct {
	// Filename indicates the file to watch.
	Filename string `hcl:"filename,attr"`
	// Type indicates how to detect changes to the file.
	Type Detector `hcl:"detector,optional"`
	// PollFrequency determines the frequency to check for changes when Type is
	// UpdateTypePoll.
	PollFrequency time.Duration `hcl:"poll_freqency,optional"`
	// IsSecret marks the file as holding a secret value which should not be
	// displayed to the user.
	IsSecret bool `hcl:"is_secret,optional"`
}

// DefaultArguments provides the default arguments for the local.file
// component.
var DefaultArguments = Arguments{
	Type:          DetectorFSNotify,
	PollFrequency: time.Minute,
}

var _ gohcl.Decoder = (*Arguments)(nil)

// DecodeHCL implements gohcl.Decoder.
func (a *Arguments) DecodeHCL(body hcl.Body, ctx *hcl.EvalContext) error {
	*a = DefaultArguments

	type arguments Arguments
	return gohcl.DecodeBody(body, ctx, (*arguments)(a))
}

// Exports holds values which are exported by the local.file component.
type Exports struct {
	// Content of the file.
	Content *hcltypes.OptionalSecret `hcl:"content,attr"`
}

// Component implements the local.file component.
type Component struct {
	opts component.Options

	mut           sync.Mutex
	args          Arguments
	latestContent string
	detector      io.Closer

	healthMut sync.RWMutex
	health    component.Health

	// reloadCh is a buffered channel which is written to when the watched file
	// should be reloaded by the component.
	reloadCh chan struct{}
}

var (
	_ component.Component       = (*Component)(nil)
	_ component.HealthComponent = (*Component)(nil)
)

// New creates a new local.file component.
func New(o component.Options, args Arguments) (*Component, error) {
	c := &Component{
		opts: o,

		reloadCh: make(chan struct{}, 1),
	}

	// Perform an update which will immediately set our exports to the initial
	// contents of the file.
	if err := c.Update(args); err != nil {
		return nil, err
	}
	return c, nil
}

// Run implements component.Component.
func (c *Component) Run(ctx context.Context) error {
	defer func() {
		c.mut.Lock()
		defer c.mut.Unlock()

		if err := c.detector.Close(); err != nil {
			level.Error(c.opts.Logger).Log("msg", "failed to shut down detector", "err", err)
		}
		c.detector = nil
	}()

	// Since Run _may_ get recalled if we're told to exit but still exist in the
	// config file, we may have prematurely destroyed the detector. If no
	// detector exists, we need to recreate it for Run to work properly.
	//
	// We ignore the error (indicating the file has disappeared) so we can allow
	// the detector to inform us when it comes back.
	//
	// TODO(rfratto): this is a design wart, and can hopefully be removed in
	// future iterations.
	c.mut.Lock()
	_ = c.configureDetector()
	c.mut.Unlock()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.reloadCh:
			time.Sleep(waitReadPeriod)

			// We ignore the error here from readFile since readFile will log errors
			// and also report the error as the health of the component.
			c.mut.Lock()
			_ = c.readFile()
			c.mut.Unlock()
		}
	}
}

func (c *Component) readFile() error {
	// Force a re-load of the file outside of the update detection mechanism.
	bb, err := os.ReadFile(c.args.Filename)
	if err != nil {
		c.setHealth(component.Health{
			Health:     component.HealthTypeUnhealthy,
			Message:    fmt.Sprintf("failed to read file: %s", err),
			UpdateTime: time.Now(),
		})
		level.Error(c.opts.Logger).Log("msg", "failed to read file", "path", c.opts.DataPath, "err", err)
		return err
	}
	c.latestContent = string(bb)

	c.opts.OnStateChange(Exports{
		Content: &hcltypes.OptionalSecret{
			IsSecret: c.args.IsSecret,
			Value:    c.latestContent,
		},
	})

	c.setHealth(component.Health{
		Health:     component.HealthTypeHealthy,
		Message:    "read file",
		UpdateTime: time.Now(),
	})
	return nil
}

// Update implements component.Compnoent.
func (c *Component) Update(args component.Arguments) error {
	newArgs := args.(Arguments)

	if newArgs.PollFrequency <= 0 {
		return fmt.Errorf("poll_freqency must be greater than 0")
	}

	c.mut.Lock()
	defer c.mut.Unlock()
	c.args = newArgs

	// Force an immediate read of the file to report any potential errors early.
	if err := c.readFile(); err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	// Each detector is dedicated to a single file path. We'll naively shut down
	// the existing detector (if any) before setting up a new one to make sure
	// the correct file is being watched in case the path changed between calls
	// to Update.
	if c.detector != nil {
		if err := c.detector.Close(); err != nil {
			level.Error(c.opts.Logger).Log("msg", "failed to shut down old detector", "err", err)
		}
		c.detector = nil
	}

	return c.configureDetector()
}

// configureDetector configures the detector if one isn't set. mut must be held
// when called.
func (c *Component) configureDetector() error {
	if c.detector != nil {
		// Already have a detector; don't do anything.
		return nil
	}

	var err error

	reloadFile := func() {
		select {
		case c.reloadCh <- struct{}{}:
		default:
			// no-op: a reload is already queued so we don't need to queue a second
			// one.
		}
	}

	switch c.args.Type {
	case DetectorPoll:
		c.detector = newPoller(pollerOptions{
			Filename:      c.args.Filename,
			ReloadFile:    reloadFile,
			PollFrequency: c.args.PollFrequency,
		})
	case DetectorFSNotify:
		c.detector, err = newFSNotify(fsNotifyOptions{
			Logger:       c.opts.Logger,
			Filename:     c.args.Filename,
			ReloadFile:   reloadFile,
			PollFreqency: c.args.PollFrequency,
		})
	}

	return err
}

// CurrentHealth implements component.HealthComponent.
func (c *Component) CurrentHealth() component.Health {
	c.healthMut.RLock()
	defer c.healthMut.RUnlock()
	return c.health
}

func (c *Component) setHealth(h component.Health) {
	c.healthMut.Lock()
	defer c.healthMut.Unlock()
	c.health = h
}
