// Copyright (c) 2017 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package job

import (
	"fmt"
	"sync"

	"github.com/vmware/harbor/src/common/dao"
	"github.com/vmware/harbor/src/common/models"
	uti "github.com/vmware/harbor/src/common/utils"
	"github.com/vmware/harbor/src/common/utils/log"
	"github.com/vmware/harbor/src/jobservice/config"
	"github.com/vmware/harbor/src/jobservice/replication"
	"github.com/vmware/harbor/src/jobservice/utils"
	"github.com/vmware/harbor/src/jobservice/driver"
)

// RepJobParm wraps the parm of a job
type RepJobParm struct {
	LocalRegURL    string

	srcURL      string
	srcUsername string
	srcPassword string
	srcAuthURL string
	srcRepository     string

	TargetURL      string
	TargetUsername string
	TargetPassword string
	TargetType int
	TargetReg Driver.Registry

	Repository     string
	Tags           []string
	Enabled        int
	Operation      string
	Insecure       bool
}

// SM is the state machine to handle job, it handles one job at a time.
type SM struct {
	JobID         int64
	CurrentState  string
	PreviousState string
	//The states that don't have to exist in transition map, such as "Error", "Canceled"
	ForcedStates map[string]struct{}
	Transitions  map[string]map[string]struct{}
	Handlers     map[string]StateHandler
	desiredState string
	Logger       *log.Logger
	Parms        *RepJobParm
	lock         *sync.Mutex
}

// EnterState transit the statemachine from the current state to the state in parameter.
// It returns the next state the statemachine should tranit to.
func (sm *SM) EnterState(s string) (string, error) {
	log.Debugf("Job id: %d, transiting from State: %s, to State: %s", sm.JobID, sm.CurrentState, s)
	targets, ok := sm.Transitions[sm.CurrentState]
	_, exist := targets[s]
	_, isForced := sm.ForcedStates[s]
	if !exist && !isForced {
		return "", fmt.Errorf("job id: %d, transition from %s to %s does not exist", sm.JobID, sm.CurrentState, s)
	}
	exitHandler, ok := sm.Handlers[sm.CurrentState]
	if ok {
		if err := exitHandler.Exit(); err != nil {
			return "", err
		}
	} else {
		log.Debugf("Job id: %d, no handler found for state:%s, skip", sm.JobID, sm.CurrentState)
	}
	enterHandler, ok := sm.Handlers[s]
	var next = models.JobContinue
	var err error
	if ok {
		if next, err = enterHandler.Enter(); err != nil {
			return "", err
		}
	} else {
		log.Debugf("Job id: %d, no handler found for state:%s, skip", sm.JobID, s)
	}
	sm.PreviousState = sm.CurrentState
	sm.CurrentState = s
	log.Debugf("Job id: %d, transition succeeded, current state: %s", sm.JobID, s)
	return next, nil
}

// Start kicks off the statemachine to transit from current state to s, and moves on
// It will search the transit map if the next state is "_continue", and
// will enter error state if there's more than one possible path when next state is "_continue"
func (sm *SM) Start(s string) {
	n, err := sm.EnterState(s)
	log.Debugf("Job id: %d, next state from handler: %s", sm.JobID, n)
	for len(n) > 0 && err == nil {
		if d := sm.getDesiredState(); len(d) > 0 {
			log.Debugf("Job id: %d. Desired state: %s, will ignore the next state from handler", sm.JobID, d)
			n = d
			sm.setDesiredState("")
			continue
		}
		if n == models.JobContinue && len(sm.Transitions[sm.CurrentState]) == 1 {
			for n = range sm.Transitions[sm.CurrentState] {
				break
			}
			log.Debugf("Job id: %d, Continue to state: %s", sm.JobID, n)
			continue
		}
		if n == models.JobContinue && len(sm.Transitions[sm.CurrentState]) != 1 {
			log.Errorf("Job id: %d, next state is continue but there are %d possible next states in transition table", sm.JobID, len(sm.Transitions[sm.CurrentState]))
			err = fmt.Errorf("Unable to continue")
			break
		}
		n, err = sm.EnterState(n)
		log.Debugf("Job id: %d, next state from handler: %s", sm.JobID, n)
	}
	if err != nil {
		log.Warningf("Job id: %d, the statemachin will enter error state due to error: %v", sm.JobID, err)
		sm.EnterState(models.JobError)
	}
}

// AddTransition add a transition to the transition table of state machine, the handler is the handler of target state "to"
func (sm *SM) AddTransition(from string, to string, h StateHandler) {
	_, ok := sm.Transitions[from]
	if !ok {
		sm.Transitions[from] = make(map[string]struct{})
	}
	sm.Transitions[from][to] = struct{}{}
	sm.Handlers[to] = h
}

// RemoveTransition removes a transition from transition table of the state machine
func (sm *SM) RemoveTransition(from string, to string) {
	_, ok := sm.Transitions[from]
	if !ok {
		return
	}
	delete(sm.Transitions[from], to)
}

// Stop will set the desired state as "stopped" such that when next tranisition happen the state machine will stop handling the current job
// and the worker can release itself to the workerpool.
func (sm *SM) Stop(id int64) {
	log.Debugf("Trying to stop the job: %d", id)
	sm.lock.Lock()
	defer sm.lock.Unlock()
	//need to check if the sm switched to other job
	if id == sm.JobID {
		sm.desiredState = models.JobStopped
		log.Debugf("Desired state of job %d is set to stopped", id)
	} else {
		log.Debugf("State machine has switched to job %d, so the action to stop job %d will be ignored", sm.JobID, id)
	}
}

func (sm *SM) getDesiredState() string {
	sm.lock.Lock()
	defer sm.lock.Unlock()
	return sm.desiredState
}

func (sm *SM) setDesiredState(s string) {
	sm.lock.Lock()
	defer sm.lock.Unlock()
	sm.desiredState = s
}

// Init initialzie the state machine, it will be called once in the lifecycle of state machine.
func (sm *SM) Init() {
	sm.lock = &sync.Mutex{}
	sm.Handlers = make(map[string]StateHandler)
	sm.Transitions = make(map[string]map[string]struct{})
	sm.ForcedStates = map[string]struct{}{
		models.JobError:    struct{}{},
		models.JobStopped:  struct{}{},
		models.JobCanceled: struct{}{},
		models.JobRetrying: struct{}{},
	}
}

// Reset resets the state machine so it will start handling another job.
func (sm *SM) Reset(jid int64) (err error) {
	//To ensure the new jobID is visible to the thread to stop the SM
	sm.lock.Lock()
	sm.JobID = jid
	sm.desiredState = ""
	sm.lock.Unlock()

	sm.Logger, err = utils.NewLogger(sm.JobID)
	if err != nil {
		return
	}
	//init parms
	job, err := dao.GetRepJob(sm.JobID)
	if err != nil {
		return fmt.Errorf("Failed to get job, error: %v", err)
	}
	if job == nil {
		return fmt.Errorf("The job doesn't exist in DB, job id: %d", sm.JobID)
	}
	policy, err := dao.GetRepPolicy(job.PolicyID)
	if err != nil {
		return fmt.Errorf("Failed to get policy, error: %v", err)
	}
	if policy == nil {
		return fmt.Errorf("The policy doesn't exist in DB, policy id:%d", job.PolicyID)
	}

	regURL, err := config.LocalRegURL()
	if err != nil {
		return err
	}
	verify, err := config.VerifyRemoteCert()
	if err != nil {
		return err
	}
	srcAuthURL := ""
	if policy.SourceID > 0{
		source, err := dao.GetRepSource(policy.SourceID)
		if err!=nil{
			return fmt.Errorf("Failed to get source, error: %v", err)
		}
		regURL = source.URL
		srcAuthURL = source.AuthURL
	}

	sm.Parms = &RepJobParm{
		LocalRegURL: regURL,
		srcAuthURL:  srcAuthURL,
		Repository:  job.Repository,
		srcRepository:job.SrcRepo,
		Tags:        job.TagList,
		Enabled:     policy.Enabled,
		Operation:   job.Operation,
		Insecure:    !verify,
	}
	if policy.Enabled == 0 {
		//worker will cancel this job
		return nil
	}
	target, err := dao.GetRepTarget(policy.TargetID)
	if err != nil {
		return fmt.Errorf("Failed to get target, error: %v", err)
	}
	if target == nil {
		return fmt.Errorf("The target doesn't exist in DB, target id: %d", policy.TargetID)
	}
	sm.Parms.TargetURL = target.URL
	sm.Parms.TargetUsername = target.Username
	sm.Parms.TargetType = target.Type
	log.Debugf("Target reg type %d",target.Type)
	switch target.Type  {
	case replication.HARBOR:
		sm.Parms.TargetReg =&Driver.HarborRegisty{
			UserName:target.Username,
			Password:target.Password,
			Url:target.URL,
			Insecure:!verify,

		}
	case replication.HUAWEI:
		sm.Parms.TargetReg =&Driver.HuaweiRegisty{
			UserName:target.Username,
			Password:target.Password,
			Url:target.URL,
			Insecure:!verify,

		}

	default:
		fmt.Errorf("the registry type %v not support now ",target.Type)
	}

	pwd := target.Password
	if len(pwd) != 0 {
		key, err := config.SecretKey()
		if err != nil {
			return err
		}
		pwd, err = uti.ReversibleDecrypt(pwd, key)
		if err != nil {
			return fmt.Errorf("failed to decrypt password: %v", err)
		}
	}

	sm.Parms.TargetPassword = pwd

	//init states handlers
	sm.Handlers = make(map[string]StateHandler)
	sm.Transitions = make(map[string]map[string]struct{})
	sm.CurrentState = models.JobPending

	sm.AddTransition(models.JobPending, models.JobRunning, StatusUpdater{sm.JobID, models.JobRunning})
	sm.AddTransition(models.JobRetrying, models.JobRunning, StatusUpdater{sm.JobID, models.JobRunning})
	sm.Handlers[models.JobError] = StatusUpdater{sm.JobID, models.JobError}
	sm.Handlers[models.JobStopped] = StatusUpdater{sm.JobID, models.JobStopped}
	sm.Handlers[models.JobRetrying] = Retry{sm.JobID}

	switch sm.Parms.Operation {
	case models.RepOpTransfer:
		addImgTransferTransition(sm)
	case models.RepOpDelete:
		addImgDeleteTransition(sm)
	default:
		err = fmt.Errorf("unsupported operation: %s", sm.Parms.Operation)
	}

	return err
}

//for testing onlly
func addTestTransition(sm *SM) error {
	sm.AddTransition(models.JobRunning, "pull-img", ImgPuller{img: sm.Parms.Repository, logger: sm.Logger})
	return nil
}

func addImgTransferTransition(sm *SM) {
	base := replication.InitBaseHandler(sm.Parms.Repository, sm.Parms.LocalRegURL, config.JobserviceSecret(),sm.Parms.srcAuthURL,sm.Parms.srcRepository,
		sm.Parms.TargetURL, sm.Parms.TargetUsername, sm.Parms.TargetPassword,sm.Parms.TargetType,sm.Parms.TargetReg,
		sm.Parms.Insecure, sm.Parms.Tags, sm.Logger)

	sm.AddTransition(models.JobRunning, replication.StateInitialize, &replication.Initializer{BaseHandler: base})
	sm.AddTransition(replication.StateInitialize, replication.StateCheck, &replication.Checker{BaseHandler: base})
	sm.AddTransition(replication.StateCheck, replication.StatePullManifest, &replication.ManifestPuller{BaseHandler: base})
	sm.AddTransition(replication.StatePullManifest, replication.StateTransferBlob, &replication.BlobTransfer{BaseHandler: base})
	sm.AddTransition(replication.StatePullManifest, models.JobFinished, &StatusUpdater{sm.JobID, models.JobFinished})
	sm.AddTransition(replication.StateTransferBlob, replication.StatePushManifest, &replication.ManifestPusher{BaseHandler: base})
	sm.AddTransition(replication.StatePushManifest, replication.StatePullManifest, &replication.ManifestPuller{BaseHandler: base})
}

func addImgDeleteTransition(sm *SM) {
	var deleter StateHandler
	log.Debugf("addImgDeleteTransition target type %d \n ",sm.Parms.TargetType)
	if sm.Parms.TargetType == replication.HUAWEI{
		log.Debug("addImgDeleteTransition huawei images \n")
		deleter = replication.NewHuaweiDeleter(sm.Parms.Repository, sm.Parms.Tags, sm.Parms.TargetURL,
			sm.Parms.TargetUsername, sm.Parms.TargetPassword, sm.Parms.Insecure, sm.Logger)
	}else{
		deleter = replication.NewDeleter(sm.Parms.Repository, sm.Parms.Tags, sm.Parms.TargetURL,
			sm.Parms.TargetUsername, sm.Parms.TargetPassword, sm.Parms.Insecure, sm.Logger)
	}


	sm.AddTransition(models.JobRunning, replication.StateDelete, deleter)
	sm.AddTransition(replication.StateDelete, models.JobFinished, &StatusUpdater{sm.JobID, models.JobFinished})
}
