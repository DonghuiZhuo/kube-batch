/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package framework

import (
	"fmt"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"

	"github.com/kubernetes-sigs/kube-batch/pkg/apis/scheduling/v1alpha1"
	"github.com/kubernetes-sigs/kube-batch/pkg/scheduler/api"
	"github.com/kubernetes-sigs/kube-batch/pkg/scheduler/cache"
	"github.com/kubernetes-sigs/kube-batch/pkg/scheduler/conf"
)

type Session struct {
	UID types.UID

	cache cache.Cache

	Jobs []*api.JobInfo
	// to keep track of topdog jobs that are borrowing resources and ready to run
	TopDogReadyJobs map[api.JobID]*api.JobInfo
	JobIndex        map[api.JobID]*api.JobInfo
	Nodes           []*api.NodeInfo
	NodeIndex       map[string]*api.NodeInfo
	Queues          []*api.QueueInfo
	QueueIndex      map[api.QueueID]*api.QueueInfo
	Others          []*api.TaskInfo
	Backlog         []*api.JobInfo
	Tiers           []conf.Tier

	plugins        map[string]Plugin
	eventHandlers  []*EventHandler
	jobOrderFns    map[string]api.CompareFn
	queueOrderFns  map[string]api.CompareFn
	taskOrderFns   map[string]api.CompareFn
	predicateFns   map[string]api.PredicateFn
	preemptableFns map[string]api.EvictableFn
	reclaimableFns map[string]api.EvictableFn
	overusedFns    map[string]api.ValidateFn
	jobReadyFns    map[string]api.ValidateFn
	jobValidFns    map[string]api.ValidateExFn
}

func openSession(cache cache.Cache) *Session {
	ssn := &Session{
		UID:        uuid.NewUUID(),
		cache:      cache,
		JobIndex:   map[api.JobID]*api.JobInfo{},
		NodeIndex:  map[string]*api.NodeInfo{},
		QueueIndex: map[api.QueueID]*api.QueueInfo{},

		plugins:        map[string]Plugin{},
		jobOrderFns:    map[string]api.CompareFn{},
		queueOrderFns:  map[string]api.CompareFn{},
		taskOrderFns:   map[string]api.CompareFn{},
		predicateFns:   map[string]api.PredicateFn{},
		preemptableFns: map[string]api.EvictableFn{},
		reclaimableFns: map[string]api.EvictableFn{},
		overusedFns:    map[string]api.ValidateFn{},
		jobReadyFns:    map[string]api.ValidateFn{},
		jobValidFns:    map[string]api.ValidateExFn{},
	}

	snapshot := cache.Snapshot()

	for _, job := range snapshot.Jobs {
		if vjr := ssn.JobValid(job); vjr != nil {
			if !vjr.Pass {
				jc := &v1alpha1.PodGroupCondition{
					Type:               v1alpha1.PodGroupUnschedulableType,
					Status:             v1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					TransitionID:       string(ssn.UID),
					Reason:             vjr.Reason,
					Message:            vjr.Message,
				}

				if err := ssn.UpdateJobCondition(job, jc); err != nil {
					glog.Errorf("Failed to update job condition: %v", err)
				}
			}

			continue
		}

		ssn.Jobs = append(ssn.Jobs, job)
	}

	for _, job := range ssn.Jobs {
		ssn.JobIndex[job.UID] = job
	}

	ssn.Nodes = snapshot.Nodes
	for _, node := range ssn.Nodes {
		ssn.NodeIndex[node.Name] = node
	}

	ssn.Queues = snapshot.Queues
	for _, queue := range ssn.Queues {
		ssn.QueueIndex[queue.UID] = queue
	}

	ssn.Others = snapshot.Others

	ssn.TopDogReadyJobs = map[api.JobID]*api.JobInfo{}

	glog.V(3).Infof("Open Session %v with <%d> Job and <%d> Queues",
		ssn.UID, len(ssn.Jobs), len(ssn.Queues))

	return ssn
}

func closeSession(ssn *Session) {
	for _, job := range ssn.Jobs {
		// If job is using PDB, ignore it.
		// TODO(k82cn): remove it when removing PDB support
		if job.PodGroup == nil {
			ssn.cache.RecordJobStatusEvent(job)
			continue
		}

		job.PodGroup.Status = jobStatus(ssn, job)
		if _, err := ssn.cache.UpdateJobStatus(job); err != nil {
			glog.Errorf("Failed to update job <%s/%s>: %v",
				job.Namespace, job.Name, err)
		}
	}

	ssn.Jobs = nil
	ssn.JobIndex = nil
	ssn.Nodes = nil
	ssn.NodeIndex = nil
	ssn.Backlog = nil
	ssn.plugins = nil
	ssn.eventHandlers = nil
	ssn.jobOrderFns = nil
	ssn.queueOrderFns = nil

	glog.V(3).Infof("Close Session %v", ssn.UID)
}

// very confusing function name!
// this updates the PodGroup.Status in the job
func jobStatus(ssn *Session, jobInfo *api.JobInfo) v1alpha1.PodGroupStatus {
	status := jobInfo.PodGroup.Status

	glog.Info("pod group %s status condition is %v", jobInfo.Name, status.Conditions)

	unschedulable := false
	for _, c := range status.Conditions {
		if c.Type == v1alpha1.PodGroupUnschedulableType &&
			c.Status == v1.ConditionTrue &&
			c.TransitionID == string(ssn.UID) {

			unschedulable = true
			break
		}
	}

	if len(jobInfo.TaskStatusIndex[api.Running]) != 0 && unschedulable {
		// If running tasks && unschedulable, unknown phase
		status.Phase = v1alpha1.PodGroupUnknown
	} else {
		// mark status to running or pending
		allocated := 0
		for status, tasks := range jobInfo.TaskStatusIndex {
			if api.AllocatedStatus(status) {
				allocated += len(tasks)
			}
		}

		// If there're enough allocated resource, it's running
		if int32(allocated) > jobInfo.PodGroup.Spec.MinMember {
			status.Phase = v1alpha1.PodGroupRunning
		} else {
			status.Phase = v1alpha1.PodGroupPending
		}
	}

	// statistics of tasks in different status
	status.Running = int32(len(jobInfo.TaskStatusIndex[api.Running]))
	status.Failed = int32(len(jobInfo.TaskStatusIndex[api.Failed]))
	status.Succeeded = int32(len(jobInfo.TaskStatusIndex[api.Succeeded]))

	return status
}

func (ssn *Session) Statement() *Statement {
	return &Statement{
		ssn: ssn,
	}
}

func (ssn *Session) Pipeline(task *api.TaskInfo, hostname string) error {
	// Only update status in session
	job, found := ssn.JobIndex[task.Job]
	if found {
		if err := job.UpdateTaskStatus(task, api.Pipelined); err != nil {
			glog.Errorf("Failed to update task <%v/%v> status to %v in Session <%v>: %v",
				task.Namespace, task.Name, api.Pipelined, ssn.UID, err)
		}
	} else {
		glog.Errorf("Failed to found Job <%s> in Session <%s> index when binding.",
			task.Job, ssn.UID)
	}

	task.NodeName = hostname

	if node, found := ssn.NodeIndex[hostname]; found {
		if err := node.AddTask(task); err != nil {
			glog.Errorf("Failed to add task <%v/%v> to node <%v> in Session <%v>: %v",
				task.Namespace, task.Name, hostname, ssn.UID, err)
		}
		glog.V(3).Infof("After added Task <%v/%v> to Node <%v>: idle <%v>, used <%v>, releasing <%v>",
			task.Namespace, task.Name, node.Name, node.Idle, node.Used, node.Releasing)
	} else {
		glog.Errorf("Failed to found Node <%s> in Session <%s> index when binding.",
			hostname, ssn.UID)
	}

	for _, eh := range ssn.eventHandlers {
		if eh.AllocateFunc != nil {
			eh.AllocateFunc(&Event{
				Task: task,
			})
		}
	}

	return nil
}

func (ssn *Session) Allocate(task *api.TaskInfo, hostname string, usingBackfillTaskRes bool) error {
	if err := ssn.cache.AllocateVolumes(task, hostname); err != nil {
		return err
	}

	// Only update status in session
	job, found := ssn.JobIndex[task.Job]
	if found {
		newStatus := api.Allocated
		if usingBackfillTaskRes {
			newStatus = api.AllocatedOverBackfill
		}
		if err := job.UpdateTaskStatus(task, newStatus); err != nil {
			glog.Errorf("Failed to update task <%v/%v> status to %v in Session <%v>: %v",
				task.Namespace, task.Name, newStatus, ssn.UID, err)
		}
	} else {
		glog.Errorf("Failed to found Job <%s> in Session <%s> index when binding.",
			task.Job, ssn.UID)
	}

	task.NodeName = hostname

	if node, found := ssn.NodeIndex[hostname]; found {
		if err := node.AddTask(task); err != nil {
			glog.Errorf("Failed to add task <%v/%v> to node <%v> in Session <%v>: %v",
				task.Namespace, task.Name, hostname, ssn.UID, err)
		}
		glog.V(3).Infof("After allocated Task <%v/%v> to Node <%v>: idle <%v>, used <%v>, releasing <%v>",
			task.Namespace, task.Name, node.Name, node.Idle, node.Used, node.Releasing)
	} else {
		glog.Errorf("Failed to found Node <%s> in Session <%s> index when binding.",
			hostname, ssn.UID)
	}

	// Callbacks
	// TODO: may need to fix (Peng)
	for _, eh := range ssn.eventHandlers {
		if eh.AllocateFunc != nil {
			eh.AllocateFunc(&Event{
				Task: task,
			})
		}
	}

	// do not dispatch when using backfilled task resource
	if ssn.JobReady(job) && !usingBackfillTaskRes {
		for _, task := range job.TaskStatusIndex[api.Allocated] {
			if err := ssn.dispatch(task); err != nil {
				glog.Errorf("Failed to dispatch task <%v/%v>: %v",
					task.Namespace, task.Name, err)
			}
		}
	} else if ssn.JobReady(job) {
		// top dog jobs using backfill resource is ready to run
		ssn.TopDogReadyJobs[job.UID] = job
	}

	return nil
}

func (ssn *Session) dispatch(task *api.TaskInfo) error {
	if err := ssn.cache.BindVolumes(task); err != nil {
		return err
	}

	if err := ssn.cache.Bind(task, task.NodeName); err != nil {
		return err
	}

	// Update status in session
	if job, found := ssn.JobIndex[task.Job]; found {
		if err := job.UpdateTaskStatus(task, api.Binding); err != nil {
			glog.Errorf("Failed to update task <%v/%v> status to %v in Session <%v>: %v",
				task.Namespace, task.Name, api.Binding, ssn.UID, err)
		}
	} else {
		glog.Errorf("Failed to found Job <%s> in Session <%s> index when binding.",
			task.Job, ssn.UID)
	}

	return nil
}

func (ssn *Session) Evict(reclaimee *api.TaskInfo, reason string) error {
	if err := ssn.cache.Evict(reclaimee, reason); err != nil {
		return err
	}

	// Update status in session
	job, found := ssn.JobIndex[reclaimee.Job]
	if found {
		if err := job.UpdateTaskStatus(reclaimee, api.Releasing); err != nil {
			glog.Errorf("Failed to update task <%v/%v> status to %v in Session <%v>: %v",
				reclaimee.Namespace, reclaimee.Name, api.Releasing, ssn.UID, err)
		}
	} else {
		glog.Errorf("Failed to found Job <%s> in Session <%s> index when binding.",
			reclaimee.Job, ssn.UID)
	}

	// Update task in node.
	if node, found := ssn.NodeIndex[reclaimee.NodeName]; found {
		if err := node.UpdateTask(reclaimee); err != nil {
			glog.Errorf("Failed to update task <%v/%v> in Session <%v>: %v",
				reclaimee.Namespace, reclaimee.Name, ssn.UID, err)
		}
	}

	for _, eh := range ssn.eventHandlers {
		if eh.DeallocateFunc != nil {
			eh.DeallocateFunc(&Event{
				Task: reclaimee,
			})
		}
	}

	return nil
}

// UpdateJobStatus update job condition accordingly.
func (ssn *Session) UpdateJobCondition(jobInfo *api.JobInfo, cond *v1alpha1.PodGroupCondition) error {
	job, ok := ssn.JobIndex[jobInfo.UID]
	if !ok {
		return fmt.Errorf("failed to find job <%s/%s>", jobInfo.Namespace, jobInfo.Name)
	}

	index := -1
	for i, c := range job.PodGroup.Status.Conditions {
		if c.Type == cond.Type {
			index = i
			break
		}
	}

	// Update condition to the new condition.
	if index < 0 {
		job.PodGroup.Status.Conditions = append(job.PodGroup.Status.Conditions, *cond)
	} else {
		job.PodGroup.Status.Conditions[index] = *cond
	}

	return nil
}

func (ssn *Session) AddEventHandler(eh *EventHandler) {
	ssn.eventHandlers = append(ssn.eventHandlers, eh)
}

func (ssn Session) String() string {
	msg := fmt.Sprintf("Session %v: \n", ssn.UID)

	for _, job := range ssn.Jobs {
		msg = fmt.Sprintf("%s%v\n", msg, job)
	}

	for _, node := range ssn.Nodes {
		msg = fmt.Sprintf("%s%v\n", msg, node)
	}

	return msg

}
