package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const dbPath = "./ant.db3"

// ScheduleType represents the type of schedule
type ScheduleType int

const (
	SingleRun ScheduleType = iota
	Repeating
)

// Schedule represents a parsed schedule
type Schedule struct {
	Type       ScheduleType
	Interval   time.Duration // for interval-based schedules (15m, 1h, etc)
	Weekday    time.Weekday  // for weekday-based schedules
	TimeOfDay  time.Time     // for specific time schedules
	IsInterval bool          // true if this is an interval-based schedule
}

// Job represents a scheduled job with Unix timestamps
type Job struct {
	ID       int
	Schedule string
	Command  string
	PID      int
	NextRun  int64 // Unix timestamp
	LastRun  int64 // Unix timestamp
}

// Initialize the database and create the jobs table if it doesn't exist
func initDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	// Only create table if it doesn't exist
	createTable := `
	CREATE TABLE IF NOT EXISTS jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		schedule TEXT,
		command TEXT,
		pid INTEGER,
		next_run INTEGER, -- Unix timestamp
		last_run INTEGER  -- Unix timestamp
	);`
	_, err = db.Exec(createTable)
	if err != nil {
		return nil, err
	}
	return db, nil
}


// AddJob inserts a new job into the database and returns its ID
func AddJob(db *sql.DB, schedule, command string, nextRun time.Time) (int64, error) {
	result, err := db.Exec(
		"INSERT INTO jobs (schedule, command, next_run, last_run, pid) VALUES (?, ?, ?, ?, ?)",
		schedule,
		command,
		nextRun.Unix(),
		0, // Initial last_run is 0
		0, // Initial PID is 0
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// UpdateJobRuns updates both the next_run and last_run times for a job
func UpdateJobRuns(db *sql.DB, jobID int, nextRun, lastRun time.Time) error {
	_, err := db.Exec(
		"UPDATE jobs SET next_run = ?, last_run = ? WHERE id = ?",
		nextRun.Unix(),
		lastRun.Unix(),
		jobID,
	)
	return err
}

// ListJobs displays all jobs stored in the database
func ListJobs(db *sql.DB) error {
	rows, err := db.Query("SELECT id, schedule, command, pid, next_run, last_run FROM jobs")
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Println("ID | Schedule | Command | PID | Next Run | Last Run")
	fmt.Println("----------------------------------------------------")
	for rows.Next() {
		var job Job
		err := rows.Scan(&job.ID, &job.Schedule, &job.Command, &job.PID, &job.NextRun, &job.LastRun)
		if err != nil {
			return err
		}
		
		nextRunTime := time.Unix(job.NextRun, 0).Format("2006-01-02 15:04:05")
		lastRunTime := "Never"
		if job.LastRun > 0 {
			lastRunTime = time.Unix(job.LastRun, 0).Format("2006-01-02 15:04:05")
		}
		
		fmt.Printf("%d | %s | %s | %d | %s | %s\n",
			job.ID, job.Schedule, job.Command, job.PID, nextRunTime, lastRunTime)
	}
	return nil
}

// parses action from args
func parseArgs(args []string) (string, string, error) {
	if len(args) < 2 {
		return "", "", fmt.Errorf("insufficient arguments")
	}

	fullArg := strings.Join(args[1:], " ")
	
	// Find the first and second colons
	firstColon := strings.Index(fullArg, ":")
	if firstColon == -1 {
		return "", "", fmt.Errorf("invalid format: missing colons")
	}
	
	lastColon := strings.LastIndex(fullArg, ":")
	if lastColon == firstColon {
		return "", "", fmt.Errorf("invalid format: missing second colon")
	}

	// Extract schedule and command
	schedule := strings.TrimSpace(fullArg[firstColon+1 : lastColon])
	command := strings.TrimSpace(fullArg[lastColon+1:])

	if schedule == "" || command == "" {
		return "", "", fmt.Errorf("invalid format: empty schedule or command")
	}

	return schedule, command, nil
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

// parseInterval handles duration-based schedules (15m, 1h, etc)
func parseInterval(input string) (time.Duration, error) {
	suffixes := map[string]time.Duration{
		"s": time.Second,
		"m": time.Minute,
		"h": time.Hour,
		"d": time.Hour * 24,
		"w": time.Hour * 24 * 7,
	}

	for suffix, unit := range suffixes {
		if strings.HasSuffix(input, suffix) {
			value := strings.TrimSuffix(input, suffix)
			if n, err := strconv.Atoi(value); err == nil {
				return time.Duration(n) * unit, nil
			}
		}
	}

	return 0, fmt.Errorf("invalid interval format: %s", input)
}

// parseWeekday converts day string to time.Weekday
func parseWeekday(day string) (time.Weekday, error) {
	days := map[string]time.Weekday{
		"sun": time.Sunday,
		"mon": time.Monday,
		"tue": time.Tuesday,
		"wed": time.Wednesday,
		"thu": time.Thursday,
		"fri": time.Friday,
		"sat": time.Saturday,
	}

	if weekday, ok := days[strings.ToLower(day)]; ok {
		return weekday, nil
	}
	return 0, fmt.Errorf("invalid weekday: %s", day)
}

// parseTimeOfDay parses time string (HHMM) into time.Time
func parseTimeOfDay(timeStr string) (time.Time, error) {
	if len(timeStr) != 4 {
		return time.Time{}, fmt.Errorf("invalid time format: %s", timeStr)
	}

	hour, err := strconv.Atoi(timeStr[:2])
	if err != nil || hour < 0 || hour > 23 {
		return time.Time{}, fmt.Errorf("invalid hour: %s", timeStr[:2])
	}

	minute, err := strconv.Atoi(timeStr[2:])
	if err != nil || minute < 0 || minute > 59 {
		return time.Time{}, fmt.Errorf("invalid minute: %s", timeStr[2:])
	}

	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location()), nil
}

// CalculateNextRun determines when the job should next run
func CalculateNextRun(schedule *Schedule) time.Time {
	now := time.Now()

	if schedule.IsInterval {
		return now.Add(schedule.Interval)
	}

	targetTime := schedule.TimeOfDay
	result := time.Date(
		now.Year(), now.Month(), now.Day(),
		targetTime.Hour(), targetTime.Minute(), 0, 0,
		now.Location(),
	)

	// Adjust the day to match the target weekday
	for result.Weekday() != schedule.Weekday {
		result = result.AddDate(0, 0, 1)
	}

	// If the calculated time is in the past, add appropriate duration
	if result.Before(now) {
		if schedule.Type == Repeating {
			// For repeating schedules, find the next occurrence
			result = result.AddDate(0, 0, 7)
		} else {
			// For single run, add days until we're in the future
			for result.Before(now) {
				result = result.AddDate(0, 0, 7)
			}
		}
	}

	return result
}

// ShowJobs spawns a tmux multipane terminal for all running jobs
func ShowJobs(db *sql.DB) error {
	rows, err := db.Query("SELECT id, command, pid FROM jobs WHERE pid > 0")
	if err != nil {
		return err
	}
	defer rows.Close()

	var runningJobs []Job
	for rows.Next() {
		var job Job
		err := rows.Scan(&job.ID, &job.Command, &job.PID)
		if err != nil {
			return err
		}
		runningJobs = append(runningJobs, job)
	}

	if len(runningJobs) == 0 {
		fmt.Println("No running jobs found.")
		return nil
	}

	// Create a new tmux session
	sessionName := "job_monitor"
	cmd := exec.Command("tmux", "new-session", "-d", "-s", sessionName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create tmux session: %v", err)
	}

	for i, job := range runningJobs {
		if i > 0 {
			cmd = exec.Command("tmux", "split-window", "-h", "-t", sessionName)
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("failed to split tmux window: %v", err)
			}
		}

		infoCommand := fmt.Sprintf("echo 'Job ID: %d | PID: %d | COMMAND: %s'", 
			job.ID, job.PID, job.Command)
		cmd = exec.Command("tmux", "send-keys", "-t", 
			fmt.Sprintf("%s:%d", sessionName, i), infoCommand, "C-m")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to send info command to tmux pane: %v", err)
		}

		tailCommand := fmt.Sprintf("tail -f nohup.%d", job.PID)
		cmd = exec.Command("tmux", "send-keys", "-t", 
			fmt.Sprintf("%s:%d", sessionName, i), tailCommand, "C-m")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to send tail command to tmux pane: %v", err)
		}
	}

	cmd = exec.Command("tmux", "attach-session", "-t", sessionName)
	return cmd.Run()
}

// DeleteJob removes a job from the database and kills its process if running
func DeleteJob(db *sql.DB, jobID int) error {
	var pid int
	err := db.QueryRow("SELECT pid FROM jobs WHERE id = ?", jobID).Scan(&pid)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("job %d not found", jobID)
		}
		return err
	}

	if pid > 0 {
		cmd := exec.Command("pkill", "-P", strconv.Itoa(pid))
		if err := cmd.Run(); err != nil {
			fmt.Printf("Warning: Failed to kill process %d: %v\n", pid, err)
		}
	}

	_, err = db.Exec("DELETE FROM jobs WHERE id = ?", jobID)
	return err
}

// StartScheduledJob starts a scheduled job and updates its PID and last_run time
func StartScheduledJob(db *sql.DB, jobID int, command string) error {
	cmd := exec.Command("bash", "-c", command)
	
	logFile, err := os.OpenFile(fmt.Sprintf("nohup.%d", jobID), 
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to create log file: %v", err)
	}
	defer logFile.Close()

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start process: %v", err)
	}

	now := time.Now()
	_, err = db.Exec(
		"UPDATE jobs SET pid = ?, last_run = ? WHERE id = ?",
		cmd.Process.Pid,
		now.Unix(),
		jobID,
	)
	if err != nil {
		cmd.Process.Kill()
		return fmt.Errorf("failed to update job status: %v", err)
	}

	cmd.Process.Release()

	fmt.Printf("Started job %d with PID %d at %s\n", 
		jobID, cmd.Process.Pid, now.Format("2006-01-02 15:04:05"))
	return nil
}

// StartWatchJob starts a job that runs every 2 seconds indefinitely
func StartWatchJob(db *sql.DB, jobID int, command string) error {
	watchScript := fmt.Sprintf(`while true; do
		%s
		sleep 2
	done`, command)

	cmd := exec.Command("bash", "-c", watchScript)
	
	logFile, err := os.OpenFile(fmt.Sprintf("nohup.%d", jobID), 
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to create log file: %v", err)
	}
	defer logFile.Close()

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start watch process: %v", err)
	}

	now := time.Now()
	_, err = db.Exec(
		"UPDATE jobs SET pid = ?, last_run = ? WHERE id = ?",
		cmd.Process.Pid,
		now.Unix(),
		jobID,
	)
	if err != nil {
		cmd.Process.Kill()
		return fmt.Errorf("failed to update job status: %v", err)
	}

	cmd.Process.Release()

	fmt.Printf("Started watch job %d with PID %d at %s\n", 
		jobID, cmd.Process.Pid, now.Format("2006-01-02 15:04:05"))

	return nil
} 

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: ant :<schedule>: <command> | ant :: <command> | ant :jobs: | ant :mon:")
		return
	}

	db, err := initDB()
	if err != nil {
		fmt.Println("Error initializing database:", err)
		return
	}
	defer db.Close()

	// Get first argument to determine action
	action := os.Args[1]

	switch {
	case action == "::":
		// Handle watch command
		if len(os.Args) < 3 {
			fmt.Println("Usage: ant :: <command>")
			return
		}
		command := strings.Join(os.Args[2:], " ")
		jobID, err := AddJob(db, "", command, time.Now())
		if err != nil {
			fmt.Println("Error adding job:", err)
			return
		}
		err = StartWatchJob(db, int(jobID), command)
		if err != nil {
			fmt.Println("Error starting watch job:", err)
		}

	case action == ":x:":
		if len(os.Args) != 3 {
			fmt.Println("Usage: ant :x: <job_id>")
			return
		}
		jobID, err := strconv.Atoi(os.Args[2])
		if err != nil {
			fmt.Printf("Invalid job ID: %v\n", err)
			return
		}
		if err := DeleteJob(db, jobID); err != nil {
			fmt.Printf("Error deleting job: %v\n", err)
			return
		}
		fmt.Printf("Job %d deleted successfully\n", jobID)

	case action == ":jobs:":
		err := ListJobs(db)
		if err != nil {
			fmt.Println("Error listing jobs:", err)
		}

	case action == ":mon:":
		err := ShowJobs(db)
		if err != nil {
			fmt.Println("Error showing jobs:", err)
		}

	default:
		// Handle scheduled commands
		schedule, command, err := parseArgs(os.Args)
		if err != nil {
			fmt.Printf("Error parsing arguments: %v\n", err)
			fmt.Println("Usage: ant :<schedule>: <command>")
			return
		}

		parsedSchedule, err := ParseSchedule(schedule)
		if err != nil {
			fmt.Printf("Error parsing schedule: %v\n", err)
			return
		}

		nextRun := CalculateNextRun(parsedSchedule)
		jobID, err := AddJob(db, schedule, command, nextRun)
		if err != nil {
			fmt.Printf("Error adding job: %v\n", err)
			return
		}
		fmt.Printf("Scheduled job %d to run at %v\n", jobID, nextRun)
	}
}