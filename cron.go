package cron

import (
	"log"
	"runtime"
	"sort"
	"sync"
	"time"
)

// Cron keeps track of any number of entries, invoking the associated func as
// specified by the schedule. It may be started, stopped, and the entries may
// be inspected while running.
type Cron struct {
	mu sync.RWMutex
	sync.Once
	entries  map[string]*Entry
	stop     chan struct{}
	add      chan *Entry
	update   chan *Entry
	snapshot chan []*Entry
	running  bool
	ErrorLog *log.Logger
	location *time.Location
}

// Job is an interface for submitted cron jobs.
type Job interface {
	Run()
}

// The Schedule describes a job's duty cycle.
type Schedule interface {
	// Return the next activation time, later than the given time.
	// Next is invoked initially, and then each time the job is run.
	Next(time.Time) time.Time
}

// Entry consists of a schedule and the func to execute on that schedule.
type Entry struct {
	// The schedule on which this job should be run.
	Schedule Schedule

	// The next time the job will run. This is the zero time if Cron has not been
	// started or this entry's schedule is unsatisfiable
	Next time.Time

	// The last time this job was run. This is the zero time if the job has never
	// been run.
	Prev time.Time

	// The Job's name
	Name string

	// The Job to run.
	Job Job
}

// byTime is a wrapper for sorting the entry array by time
// (with zero time at the end).
type byTime []*Entry

func (s byTime) Len() int      { return len(s) }
func (s byTime) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s byTime) Less(i, j int) bool {
	// Two zero times should return false.
	// Otherwise, zero is "greater" than any other time.
	// (To sort it at the end of the list.)
	if s[i].Next.IsZero() {
		return false
	}
	if s[j].Next.IsZero() {
		return true
	}
	return s[i].Next.Before(s[j].Next)
}

// New returns a new Cron job runner, in the Local time zone.
func New() *Cron {
	return NewWithLocation(time.Now().Location())
}

// NewWithLocation returns a new Cron job runner.
func NewWithLocation(location *time.Location) *Cron {
	return &Cron{
		entries:  make(map[string]*Entry),
		add:      make(chan *Entry),
		update:   make(chan *Entry),
		stop:     make(chan struct{}),
		snapshot: make(chan []*Entry),
		running:  false,
		ErrorLog: nil,
		location: location,
	}
}

// A wrapper that turns a func() into a cron.Job
type FuncJob func()

func (f FuncJob) Run() { f() }

// AddFunc adds a func to the Cron to be run on the given schedule.
func (c *Cron) AddFunc(spec, name string, cmd func()) error {
	return c.AddJob(spec, name, FuncJob(cmd))
}

// UpdateFunc update a func to the Cron to be run on the given schedule by name.
func (c *Cron) UpdateFunc(spec, name string, cmd func()) error {
	return c.UpdateJob(spec, name, FuncJob(cmd))
}

// AddJob adds a Job to the Cron to be run on the given schedule.
func (c *Cron) AddJob(spec, name string, cmd Job) error {
	schedule, err := Parse(spec)
	if err != nil {
		return err
	}
	c.Schedule(schedule, name, cmd, false)
	return nil
}

// UpdateJob update a Job to the Cron to be run on the given schedule by name.
func (c *Cron) UpdateJob(spec, name string, cmd Job) error {
	schedule, err := Parse(spec)
	if err != nil {
		return err
	}
	c.Schedule(schedule, name, cmd, true)
	return nil
}

// RemoveJobOrFunc remove a job or func from the Cron to be run on the given schedule.
func (c *Cron) RemoveJobOrFunc(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries != nil {
		delete(c.entries, name)
	}
	return
}

// Schedule adds a Job to the Cron to be run on the given schedule.
func (c *Cron) Schedule(schedule Schedule, name string, cmd Job, update bool) {
	c.init()
	entry := &Entry{
		Schedule: schedule,
		Job:      cmd,
		Name:     name,
	}
	if !c.running {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, has := c.entries[name]
		if !has {
			c.entries[name] = entry
		}
		return
	}

	if update {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.entries[name] = entry
		c.update <- entry
		return
	}
	c.add <- entry
}

// Entries returns a snapshot of the cron entries.
func (c *Cron) Entries() []*Entry {
	c.init()
	if c.running {
		c.snapshot <- nil
		x := <-c.snapshot
		return x
	}
	return c.entrySnapshot()
}

// Location gets the time zone location
func (c *Cron) Location() *time.Location {
	return c.location
}

// Start the cron scheduler in its own go-routine, or no-op if already started.
func (c *Cron) Start() {
	if c.running {
		return
	}
	c.running = true
	go c.run()
}

// Run the cron scheduler, or no-op if already running.
func (c *Cron) Run() {
	if c.running {
		return
	}
	c.running = true
	c.run()
}

func (c *Cron) runWithRecovery(j Job) {
	defer func() {
		if r := recover(); r != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			c.logf("cron: panic running job: %v\n%s", r, buf)
		}
	}()
	j.Run()
}

// Run the scheduler. this is private just due to the need to synchronize
// access to the 'running' state variable.
func (c *Cron) run() {
	c.init()
	// Figure out the next activation times for each entry.
	now := c.now()
	c.mu.RLock()
	for _, entry := range c.entries {
		entry.Next = entry.Schedule.Next(now)
	}
	c.mu.RUnlock()

	for {
		// Determine the next entry to run.
		var entries []*Entry
		c.mu.RLock()
		entries = mapToSlice(c.entries)
		c.mu.RUnlock()
		sort.Sort(byTime(entries))

		var timer *time.Timer
		if len(entries) == 0 || entries[0].Next.IsZero() {
			// If there are no entries yet, just sleep - it still handles new entries
			// and stop requests.
			timer = time.NewTimer(100000 * time.Hour)
		} else {
			timer = time.NewTimer(entries[0].Next.Sub(now))
		}

		for {
			select {
			case now = <-timer.C:
				now = now.In(c.location)
				// Run every entry whose next time was less than now
				for _, e := range entries {
					if e.Next.After(now) || e.Next.IsZero() {
						break
					}
					go c.runWithRecovery(e.Job)
					e.Prev = e.Next
					e.Next = e.Schedule.Next(now)
				}

			case newEntry := <-c.add:
				timer.Stop()
				now = c.now()
				newEntry.Next = newEntry.Schedule.Next(now)
				c.mu.Lock()
				_, has := c.entries[newEntry.Name]
				if !has {
					c.entries[newEntry.Name] = newEntry
				}
				c.mu.Unlock()

			case newEntry := <-c.update:
				timer.Stop()
				now = c.now()
				newEntry.Next = newEntry.Schedule.Next(now)
				c.mu.Lock()
				c.entries[newEntry.Name] = newEntry
				c.mu.Unlock()

			case <-c.snapshot:
				c.snapshot <- c.entrySnapshot()
				continue

			case <-c.stop:
				timer.Stop()
				return
			}

			break
		}
	}
}

// Logs an error to stderr or to the configured error log.
func (c *Cron) logf(format string, args ...interface{}) {
	if c.ErrorLog != nil {
		c.ErrorLog.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// Stop stops the cron scheduler if it is running; otherwise it does nothing.
func (c *Cron) Stop() {
	if !c.running {
		return
	}
	c.stop <- struct{}{}
	c.running = false
}

// entrySnapshot returns a copy of the current cron entry list.
func (c *Cron) entrySnapshot() []*Entry {
	entries := []*Entry{}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, e := range c.entries {
		entries = append(entries, &Entry{
			Schedule: e.Schedule,
			Next:     e.Next,
			Prev:     e.Prev,
			Job:      e.Job,
		})
	}
	return entries
}

// now returns current time in c location.
func (c *Cron) now() time.Time {
	return time.Now().In(c.location)
}

// init cron entries once.
func (c *Cron) init() {
	c.Once.Do(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.entries == nil {
			c.entries = make(map[string]*Entry)
		}
	})
}
