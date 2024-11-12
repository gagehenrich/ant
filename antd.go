package main

import (
	"database/sql"
	"daemon"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ScheduleType represents the type of schedule (single run or repeating)
type ScheduleType int

const (
	SingleRun ScheduleType = iota
	Repeating
)

// Schedule represents a parsed schedule configuration
type Schedule struct {
	Type      ScheduleType
	Weekday   time.Weekday
	TimeOfDay time.Time
	Interval  time.Duration
	IsInterval bool
}

// Helper function to parse intervals like "15m", "1h", etc.
func parseInterval(input string) (time.Duration, error) {
	return time.ParseDuration(input)
}

// Helper function to parse weekday strings
func parseWeekday(input string) (time.Weekday, error) {
	weekdays := map[string]time.Weekday{
		"sun": time.Sunday,
		"mon": time.Monday,
		"tue": time.Tuesday,
		"wed": time.Wednesday,
		"thu": time.Thursday,
		"fri": time.Friday,
		"sat": time.Saturday,
	}

	day, exists := weekdays[strings.ToLower(input)]
	if !exists {
		return 0, fmt.Errorf("invalid weekday: %s", input)
	}
	return day, nil
}

// Helper function to parse time strings like "15:04"
func parseTimeOfDay(input string) (time.Time, error) {
	t, err := time.Parse("15:04", input)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid time format: %s", input)
	}
	return t, nil
}

const (
	dbPath        = "./ant.db3"
	pollInterval  = 1 * time.Second
	logTimeFormat = "2006-01-02 15:04:05"
)

// Job represents a scheduled job with Unix timestamps
type Job struct {
	ID       int
	Schedule string
	Command  string
	PID      int
	NextRun  int64
	LastRun  int64
}

type Daemon struct {
	db        *sql.DB
	logger    *log.Logger
	wg        sync.WaitGroup
	stopChan  chan struct{}
	jobsMutex sync.Mutex
}

func NewDaemon(db *sql.DB) *Daemon {
	// Create logger
	logFile, err := os.OpenFile("antd.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal("Failed to open log file:", err)
	}

	logger := log.New(logFile, "", log.LstdFlags)

	return &Daemon{
		db:       db,
		logger:   logger,
		stopChan: make(chan struct{}),
	}
}

func (d *Daemon) Start() {
	d.logger.Println("Daemon starting...")
	
	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start the main monitoring loop
	d.wg.Add(1)
	go d.monitorJobs()

	// Wait for shutdown signal
	sig := <-sigChan
	d.logger.Printf("Received signal %v, shutting down...", sig)
	close(d.stopChan)
	d.wg.Wait()
	d.logger.Println("Daemon stopped")
}

func (d *Daemon) monitorJobs() {
	defer d.wg.Done()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopChan:
			return
		case <-ticker.C:
			if err := d.checkAndExecuteJobs(); err != nil {
				d.logger.Printf("Error checking jobs: %v", err)
			}
		}
	}
}

func (d *Daemon) checkAndExecuteJobs() error {
	d.jobsMutex.Lock()
	defer d.jobsMutex.Unlock()

	now := time.Now().Unix()
	
	// Query for jobs that are due to run
	rows, err := d.db.Query(`
		SELECT id, schedule, command, pid, next_run, last_run 
		FROM jobs 
		WHERE next_run <= ? AND (pid = 0 OR pid IS NULL)`,
		now,
	)
	if err != nil {
		return fmt.Errorf("query failed: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var job Job
		err := rows.Scan(&job.ID, &job.Schedule, &job.Command, &job.PID, &job.NextRun, &job.LastRun)
		if err != nil {
			d.logger.Printf("Error scanning job: %v", err)
			continue
		}

		// Execute the job
		if err := d.executeJob(&job); err != nil {
			d.logger.Printf("Error executing job %d: %v", job.ID, err)
			continue
		}

		// Calculate and update the next run time if it's a repeating job
		if err := d.updateJobSchedule(&job); err != nil {
			d.logger.Printf("Error updating job %d schedule: %v", job.ID, err)
		}
	}

	return nil
}

func (d *Daemon) executeJob(job *Job) error {
	d.logger.Printf("Executing job %d: %s", job.ID, job.Command)

	// Create log file for the job
	logFile, err := os.OpenFile(
		fmt.Sprintf("nohup.%d", job.ID),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY,
		0644,
	)
	if err != nil {
		return fmt.Errorf("failed to create log file: %v", err)
	}
	defer logFile.Close()

	// Prepare command
	cmd := exec.Command("bash", "-c", job.Command)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	
	// Set working directory to the same directory as the database
	cmd.Dir = filepath.Dir(dbPath)

	// Start the command
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start process: %v", err)
	}

	// Update job status in database
	now := time.Now().Unix()
	_, err = d.db.Exec(
		"UPDATE jobs SET pid = ?, last_run = ? WHERE id = ?",
		cmd.Process.Pid,
		now,
		job.ID,
	)
	if err != nil {
		cmd.Process.Kill()
		return fmt.Errorf("failed to update job status: %v", err)
	}

	// Start a goroutine to monitor the process completion
	go func() {
		cmd.Wait()
		d.jobsMutex.Lock()
		defer d.jobsMutex.Unlock()
		
		_, err := d.db.Exec("UPDATE jobs SET pid = 0 WHERE id = ?", job.ID)
		if err != nil {
			d.logger.Printf("Error updating job %d PID after completion: %v", job.ID, err)
		}
	}()

	d.logger.Printf("Started job %d with PID %d", job.ID, cmd.Process.Pid)
	return nil
}

func (d *Daemon) updateJobSchedule(job *Job) error {
	if job.Schedule == "" {
		// Non-repeating job, no need to update schedule
		return nil
	}

	// Parse the schedule string
	schedule, err := ParseSchedule(job.Schedule)
	if err != nil {
		return fmt.Errorf("failed to parse schedule: %v", err)
	}

	// Calculate next run time
	nextRun := CalculateNextRun(schedule)

	// Update the database
	_, err = d.db.Exec(
		"UPDATE jobs SET next_run = ? WHERE id = ?",
		nextRun.Unix(),
		job.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update next run time: %v", err)
	}

	d.logger.Printf("Updated job %d next run time to %s", job.ID, nextRun.Format(logTimeFormat))
	return nil
}

// ParseSchedule parses schedule strings into a Schedule struct
func ParseSchedule(input string) (*Schedule, error) {
	input = strings.TrimSpace(input)
	schedule := &Schedule{}

	// Check if it's a repeating schedule
	if strings.HasPrefix(input, "e ") {
		schedule.Type = Repeating
		input = strings.TrimPrefix(input, "e ")
	} else {
		schedule.Type = SingleRun
	}

	// Try to parse as an interval first (15m, 1h, etc)
	if duration, err := parseInterval(input); err == nil {
		schedule.Interval = duration
		schedule.IsInterval = true
		return schedule, nil
	}

	// Split remaining input into day and time parts
	parts := strings.Fields(input)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid schedule format: %s", input)
	}

	// Parse weekday
	weekday, err := parseWeekday(parts[0])
	if err != nil {
		return nil, err
	}
	schedule.Weekday = weekday

	// Parse time
	timeOfDay, err := parseTimeOfDay(parts[1])
	if err != nil {
		return nil, err
	}
	schedule.TimeOfDay = timeOfDay

	return schedule, nil
}

func main() {
	// Set up logging to work with systemd
	logger := log.New(os.Stdout, "", log.LstdFlags)
	
	// Update default paths for systemd service
	dbPath := "/var/lib/antd/ant.db3"
	
	// Open database connection
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		logger.Fatal("Failed to open database:", err)
	}
	defer db.Close()

	// Send startup notification to systemd
	if os.Getenv("NOTIFY_SOCKET") != "" {
		daemon.SdNotify(false, daemon.SdNotifyReady)
	}

	// Create and start the daemon
	d := NewDaemon(db)
	
	// Update logger to use systemd's stdout
	d.logger = logger
	
	// Start the daemon
	d.Start()

	// Notify systemd of clean shutdown
	if os.Getenv("NOTIFY_SOCKET") != "" {
		daemon.SdNotify(false, daemon.SdNotifyStopping)
	}
}