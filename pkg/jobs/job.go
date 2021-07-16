package jobs

import (
	"bytes"
	"fmt"
	"time"

	api "github.com/semaphoreci/agent/pkg/api"
	eventlogger "github.com/semaphoreci/agent/pkg/eventlogger"
	executors "github.com/semaphoreci/agent/pkg/executors"
	pester "github.com/sethgrid/pester"
	log "github.com/sirupsen/logrus"
)

const JOB_PASSED = "passed"
const JOB_FAILED = "failed"

type Job struct {
	Request *api.JobRequest
	Logger  *eventlogger.Logger

	Executor executors.Executor

	JobLogArchived bool
	Stopped        bool
	Finished       bool

	//
	// The Teardown phase can be entered either after:
	//  - a regular job execution ends
	//  - from the Job.Stop procedure
	//
	// With this lock, we are making sure that only one Teardown is
	// executed. This solves the race condition where both the job finishes
	// and the job stops at the same time.
	//
	TeardownLock Lock
}

func NewJob(request *api.JobRequest) (*Job, error) {
	if request.Executor == "" {
		log.Infof("No executor specified - using %s executor", executors.ExecutorTypeShell)
		request.Executor = executors.ExecutorTypeShell
	}

	if request.Logger.Method == "" {
		log.Infof("No logger method specified - using %s logger method", eventlogger.LoggerMethodPull)
		request.Logger.Method = eventlogger.LoggerMethodPull
	}

	logger, err := eventlogger.CreateLogger(request)
	if err != nil {
		return nil, err
	}

	executor, err := executors.CreateExecutor(request, logger)
	if err != nil {
		return nil, err
	}

	return &Job{
		Request:        request,
		Executor:       executor,
		JobLogArchived: false,
		Stopped:        false,
		Logger:         logger,
	}, nil
}

func (job *Job) Run(finishedCallback func()) {
	log.Infof("Running job %s", job.Request.ID)
	executorRunning := false
	result := JOB_FAILED

	job.Logger.LogJobStarted()

	exitCode := job.PrepareEnvironment()
	if exitCode == 0 {
		executorRunning = true
	} else {
		log.Error("Executor failed to boot up")
	}

	if executorRunning {
		result = job.RunRegularCommands()

		if result == JOB_PASSED {
			log.Info("Regular commands finished successfully")
		} else {
			log.Info("Regular commands finished with failure")
		}

		log.Debug("Exporting job result")

		job.RunCommandsUntilFirstFailure([]api.Command{
			{
				Directive: fmt.Sprintf("export SEMAPHORE_JOB_RESULT=%s", result),
			},
		})

		log.Info("Starting epilogue always commands")
		job.RunCommandsUntilFirstFailure(job.Request.EpilogueAlwaysCommands)

		if result == JOB_PASSED {
			log.Info("Starting epilogue on pass commands")
			job.RunCommandsUntilFirstFailure(job.Request.EpilogueOnPassCommands)
		} else {
			log.Info("Starting epilogue on fail commands")
			job.RunCommandsUntilFirstFailure(job.Request.EpilogueOnFailCommands)
		}
	}

	job.Teardown(result)
	job.Finished = true

	if finishedCallback != nil {
		finishedCallback()
	}
}

func (job *Job) PrepareEnvironment() int {
	exitCode := job.Executor.Prepare()
	if exitCode != 0 {
		log.Error("Failed to prepare executor")
		return exitCode
	}

	exitCode = job.Executor.Start()
	if exitCode != 0 {
		log.Error("Failed to start executor")
		return exitCode
	}

	return 0
}

func (job *Job) RunRegularCommands() string {
	exitCode := job.Executor.ExportEnvVars(job.Request.EnvVars)
	if exitCode != 0 {
		log.Error("Failed to export env vars")

		return JOB_FAILED
	}

	exitCode = job.Executor.InjectFiles(job.Request.Files)
	if exitCode != 0 {
		log.Error("Failed to inject files")

		return JOB_FAILED
	}

	exitCode = job.RunCommandsUntilFirstFailure(job.Request.Commands)

	if exitCode == 0 {
		return JOB_PASSED
	} else {
		return JOB_FAILED
	}
}

// returns exit code of last executed command
func (job *Job) RunCommandsUntilFirstFailure(commands []api.Command) int {
	lastExitCode := 1

	for _, c := range commands {
		if job.Stopped {
			return 1
		}

		lastExitCode = job.Executor.RunCommand(c.Directive, false, c.Alias)

		if lastExitCode != 0 {
			break
		}
	}

	return lastExitCode
}

func (job *Job) Teardown(result string) {
	if !job.TeardownLock.TryLock() {
		log.Warning("Duplicate attempts to enter the Teardown phase")
		return
	}

	log.Debug("Sending finished callback")
	job.SendFinishedCallback(result)
	job.Logger.LogJobFinished(result)

	if job.Request.Logger.Method == eventlogger.LoggerMethodPull {
		log.Debug("Waiting for archivator")

		for {
			if job.JobLogArchived {
				break
			} else {
				time.Sleep(1000 * time.Millisecond)
			}
		}

		log.Debug("Archivator finished")
	}

	err := job.Logger.Close()
	if err != nil {
		log.Errorf("Error closing logger: %+v", err)
	}

	job.SendTeardownFinishedCallback()

	log.Info("Job teardown finished")
}

func (j *Job) Stop() {
	log.Info("Stopping job")

	j.Stopped = true

	log.Debug("Invoking process stopping")

	PreventPanicPropagation(func() {
		j.Executor.Stop()
	})

	j.Teardown("stopped")
}

func (job *Job) SendFinishedCallback(result string) error {
	payload := fmt.Sprintf(`{"result": "%s"}`, result)

	return job.SendCallback(job.Request.Callbacks.Finished, payload)
}

func (job *Job) SendTeardownFinishedCallback() error {
	return job.SendCallback(job.Request.Callbacks.TeardownFinished, "{}")
}

func (job *Job) SendCallback(url string, payload string) error {
	log.Debugf("Sending callback: %s with %+v", url, payload)

	client := pester.New()
	client.MaxRetries = 100
	client.KeepLog = true

	resp, err := client.Post(url, "application/json", bytes.NewBuffer([]byte(payload)))

	log.Debugf("Callback response: %+v", resp)

	return err
}
