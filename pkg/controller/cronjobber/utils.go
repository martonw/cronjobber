/*
Copyright 2016 The Kubernetes Authors.
Copyright 2019 The Cronjobber Authors.

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

package cronjobber

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	cronjobberv1 "github.com/hiddeco/cronjobber/pkg/apis/cronjobber/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Utilities for dealing with Jobs and CronJobs and time.

func inActiveList(sj cronjobberv1.TZCronJob, uid types.UID) bool {
	for _, j := range sj.Status.Active {
		if j.UID == uid {
			return true
		}
	}
	return false
}

func deleteFromActiveList(sj *cronjobberv1.TZCronJob, uid types.UID) {
	if sj == nil {
		return
	}
	newActive := []v1.ObjectReference{}
	for _, j := range sj.Status.Active {
		if j.UID != uid {
			newActive = append(newActive, j)
		}
	}
	sj.Status.Active = newActive
}

// getParentUIDFromJob extracts UID of job's parent and whether it was found
func getParentUIDFromJob(j batchv1.Job, logger *zap.SugaredLogger) (types.UID, bool) {
	controllerRef := metav1.GetControllerOf(&j)

	if controllerRef == nil {
		return types.UID(""), false
	}

	if controllerRef.Kind != "TZCronJob" {
		logger.Debugf("Job with non-TZCronJob parent, name %s namespace %s", j.Name, j.Namespace)
		return types.UID(""), false
	}

	return controllerRef.UID, true
}

// groupJobsByParent groups jobs into a map keyed by the job parent UID (e.g. scheduledJob).
// It has no receiver, to facilitate testing.
func groupJobsByParent(js []batchv1.Job, logger *zap.SugaredLogger) map[types.UID][]batchv1.Job {
	jobsBySj := make(map[types.UID][]batchv1.Job)
	for _, job := range js {
		parentUID, found := getParentUIDFromJob(job, logger)
		if !found {
			logger.Debugf("Unable to get parent uid from job %s in namespace %s", job.Name, job.Namespace)
			continue
		}
		jobsBySj[parentUID] = append(jobsBySj[parentUID], job)
	}
	return jobsBySj
}

// getRecentUnmetScheduleTimes gets a slice of times (from oldest to latest) that have passed when a Job should have started but did not.
//
// If there are too many (>100) unstarted times, just give up and return an empty slice.
// If there were missed times prior to the last known start time, then those are not returned.
func getRecentUnmetScheduleTimes(sj cronjobberv1.TZCronJob, now time.Time) ([]time.Time, error) {
	starts := []time.Time{}
	sched, err := cron.ParseStandard(sj.Spec.Schedule)
	if err != nil {
		return starts, fmt.Errorf("Unparseable schedule: %s : %s", sj.Spec.Schedule, err)
	}

	var earliestTime time.Time
	if sj.Status.LastScheduleTime != nil {
		earliestTime = sj.Status.LastScheduleTime.Time.In(now.Location())
	} else {
		// If none found, then this is either a recently created scheduledJob,
		// or the active/completed info was somehow lost (contract for status
		// in kubernetes says it may need to be recreated), or that we have
		// started a job, but have not noticed it yet (distributed systems can
		// have arbitrary delays).  In any case, use the creation time of the
		// TZCronJob as last known start time.
		earliestTime = sj.ObjectMeta.CreationTimestamp.Time.In(now.Location())
	}
	if sj.Spec.StartingDeadlineSeconds != nil {
		// Controller is not going to schedule anything below this point
		schedulingDeadline := now.Add(-time.Second * time.Duration(*sj.Spec.StartingDeadlineSeconds)).In(now.Location())

		if schedulingDeadline.After(earliestTime) {
			earliestTime = schedulingDeadline
		}
	}
	if earliestTime.After(now) {
		return []time.Time{}, nil
	}

	for t := sched.Next(earliestTime); !t.After(now); t = sched.Next(t) {
		starts = append(starts, t)
		// An object might miss several starts. For example, if
		// controller gets wedged on friday at 5:01pm when everyone has
		// gone home, and someone comes in on tuesday AM and discovers
		// the problem and restarts the controller, then all the hourly
		// jobs, more than 80 of them for one hourly scheduledJob, should
		// all start running with no further intervention (if the scheduledJob
		// allows concurrency and late starts).
		//
		// However, if there is a bug somewhere, or incorrect clock
		// on controller's server or apiservers (for setting creationTimestamp)
		// then there could be so many missed start times (it could be off
		// by decades or more), that it would eat up all the CPU and memory
		// of this controller. In that case, we want to not try to list
		// all the missed start times.
		//
		// I've somewhat arbitrarily picked 100, as more than 80,
		// but less than "lots".
		if len(starts) > 100 {
			// We can't get the most recent times so just return an empty slice
			return []time.Time{}, fmt.Errorf("too many missed start time (> 100). Set or decrease .spec.startingDeadlineSeconds or check clock skew")
		}
	}
	return starts, nil
}

// getJobFromTemplate makes a Job from a CronJob
func getJobFromTemplate(sj *cronjobberv1.TZCronJob, scheduledTime time.Time) (*batchv1.Job, error) {
	labels := copyLabels(&sj.Spec.JobTemplate)
	annotations := copyAnnotations(&sj.Spec.JobTemplate)
	// We want job names for a given nominal start time to have a deterministic name to avoid the same job being created twice
	name := fmt.Sprintf("%s-%d", sj.Name, getTimeHash(scheduledTime))

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Labels:          labels,
			Annotations:     annotations,
			Name:            name,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(sj, controllerKind)},
		},
		Spec: sj.Spec.JobTemplate.Spec,
	}
	return job, nil
}

// getTimeHash returns Unix Epoch Time
func getTimeHash(scheduledTime time.Time) int64 {
	return scheduledTime.Unix()
}

func getFinishedStatus(j *batchv1.Job) (bool, batchv1.JobConditionType) {
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == v1.ConditionTrue {
			return true, c.Type
		}
	}
	return false, ""
}

func IsJobFinished(j *batchv1.Job) bool {
	isFinished, _ := getFinishedStatus(j)
	return isFinished
}

// byJobStartTime sorts a list of jobs by start timestamp, using their names as a tie breaker.
type byJobStartTime []batchv1.Job

func (o byJobStartTime) Len() int      { return len(o) }
func (o byJobStartTime) Swap(i, j int) { o[i], o[j] = o[j], o[i] }

func (o byJobStartTime) Less(i, j int) bool {
	if o[j].Status.StartTime == nil {
		return o[i].Status.StartTime != nil
	}

	if o[i].Status.StartTime.Equal(o[j].Status.StartTime) {
		return o[i].Name < o[j].Name
	}

	return o[i].Status.StartTime.Before(o[j].Status.StartTime)
}

func getCurrentTimeInZone(sj *cronjobberv1.TZCronJob) (time.Time, error) {
	if sj.Spec.TimeZone == "" {
		return time.Now(), nil
	}

	loc, err := time.LoadLocation(sj.Spec.TimeZone)
	if err != nil {
		return time.Now(), err
	}

	return time.Now().In(loc), nil
}
