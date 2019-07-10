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

package override

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jenkins-x/go-scm/scm"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/jenkins-x/lighthouse/pkg/prow/config"
	"github.com/jenkins-x/lighthouse/pkg/prow/github"
	prowapi "k8s.io/test-infra/prow/apis/plumberJobs/v1"
)

const (
	fakeOrg     = "fake-org"
	fakeRepo    = "fake-repo"
	fakePR      = 33
	fakeSHA     = "deadbeef"
	fakeBaseSHA = "fffffff"
	adminUser   = "admin-user"
)

type fakeClient struct {
	comments   []string
	statuses   map[string]scm.Status
	presubmits map[string]config.Presubmit
	jobs       sets.String
}

func (c *fakeClient) CreateComment(org, repo string, number int, comment string) error {
	switch {
	case org != fakeOrg:
		return fmt.Errorf("bad org: %s", org)
	case repo != fakeRepo:
		return fmt.Errorf("bad repo: %s", repo)
	case number != fakePR:
		return fmt.Errorf("bad number: %d", number)
	case strings.Contains(comment, "fail-comment"):
		return errors.New("injected CreateComment failure")
	}
	c.comments = append(c.comments, comment)
	return nil
}

func (c *fakeClient) CreateStatus(org, repo, ref string, s scm.Status) error {
	switch {
	case s.Context == "fail-create":
		return errors.New("injected CreateStatus failure")
	case org != fakeOrg:
		return fmt.Errorf("bad org: %s", org)
	case repo != fakeRepo:
		return fmt.Errorf("bad repo: %s", repo)
	case ref != fakeSha:
		return fmt.Errorf("bad ref: %s", ref)
	}
	c.statuses[s.Context] = s
	return nil
}

func (c *fakeClient) GetPullRequest(org, repo string, number int) (*scm.PullRequest, error) {
	switch {
	case number < 0:
		return nil, errors.New("injected GetPullRequest failure")
	case org != fakeOrg:
		return nil, fmt.Errorf("bad org: %s", org)
	case repo != fakeRepo:
		return nil, fmt.Errorf("bad repo: %s", repo)
	case number != fakePR:
		return nil, fmt.Errorf("bad number: %d", number)
	}
	var pr scm.PullRequest
	pr.Sha = fakeSHA
	return &pr, nil
}

func (c *fakeClient) ListStatuses(org, repo, ref string) ([]*scm.Status, error) {
	switch {
	case org != fakeOrg:
		return nil, fmt.Errorf("bad org: %s", org)
	case repo != fakeRepo:
		return nil, fmt.Errorf("bad repo: %s", repo)
	case ref != fakeSha:
		return nil, fmt.Errorf("bad ref: %s", ref)
	}
	var out []*scm.Status
	for _, s := range c.statuses {
		if s.Context == "fail-list" {
			return nil, errors.New("injected ListStatuses failure")
		}
		out = append(out, s)
	}
	return out, nil
}

func (c *fakeClient) HasPermission(org, repo, user string, roles ...string) (bool, error) {
	switch {
	case org != fakeOrg:
		return false, fmt.Errorf("bad org: %s", org)
	case repo != fakeRepo:
		return false, fmt.Errorf("bad repo: %s", repo)
	case roles[0] != github.RoleAdmin:
		return false, fmt.Errorf("bad roles: %s", roles)
	case user == "fail":
		return true, errors.New("injected HasPermission error")
	}
	return user == adminUser, nil
}

func (c *fakeClient) GetRef(org, repo, ref string) (string, error) {
	if repo == "fail-ref" {
		return "", errors.New("injected GetRef error")
	}
	return fakeBaseSHA, nil
}

func (c *fakeClient) Create(pj *builder.PlumberJob) (*builder.PlumberJob, error) {
	if s := pj.Status.State; s != builder.SuccessState {
		return pj, fmt.Errorf("bad status state: %s", s)
	}
	if pj.Spec.Context == "fail-create" {
		return pj, errors.New("injected CreatePlumberJob error")
	}
	c.jobs.Insert(pj.Spec.Context)
	return pj, nil
}

func (c *fakeClient) presubmitForContext(org, repo, context string) *config.Presubmit {
	p, ok := c.presubmits[context]
	if !ok {
		return nil
	}
	return &p
}

func TestAuthorized(t *testing.T) {
	cases := []struct {
		name     string
		user     string
		expected bool
	}{
		{
			name: "fail closed",
			user: "fail",
		},
		{
			name: "reject rando",
			user: "random",
		},
		{
			name:     "accept admin",
			user:     adminUser,
			expected: true,
		},
	}

	log := logrus.WithField("plugin", pluginName)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if actual := authorized(&fakeClient{}, log, fakeOrg, fakeRepo, tc.user); actual != tc.expected {
				t.Errorf("actual %t != expected %t", actual, tc.expected)
			}
		})
	}
}

func TestHandle(t *testing.T) {
	cases := []struct {
		name          string
		action        scm.Action
		issue         bool
		state         string
		comment       string
		contexts      map[string]scm.Status
		presubmits    map[string]config.Presubmit
		user          string
		number        int
		expected      map[string]scm.Status
		jobs          sets.String
		checkComments []string
		err           bool
	}{
		{
			name:    "successfully override failure",
			comment: "/override broken-test",
			contexts: map[string]scm.Status{
				"broken-test": {
					Context: "broken-test",
					State:   scm.StatusFailure,
				},
			},
			expected: map[string]scm.Status{
				"broken-test": {
					Context:     "broken-test",
					Description: description(adminUser),
					State:       scm.StateSuccess,
				},
			},
			checkComments: []string{"on behalf of " + adminUser},
		},
		{
			name:    "successfully override pending",
			comment: "/override hung-test",
			contexts: map[string]scm.Status{
				"hung-test": {
					Context: "hung-test",
					State:   scm.StatusPending,
				},
			},
			expected: map[string]scm.Status{
				"hung-test": {
					Context:     "hung-test",
					Description: description(adminUser),
					State:       scm.StateSuccess,
				},
			},
		},
		{
			name:    "comment for incorrect context",
			comment: "/override whatever-you-want",
			contexts: map[string]scm.Status{
				"hung-test": {
					Context: "hung-test",
					State:   scm.StatusPending,
				},
			},
			expected: map[string]scm.Status{
				"hung-test": {
					Context: "hung-test",
					State:   scm.StatusPending,
				},
			},
			checkComments: []string{
				"The following unknown contexts were given", "whatever-you-want",
				"Only the following contexts were expected", "hung-context",
			},
		},
		{
			name:    "refuse override from non-admin",
			comment: "/override broken-test",
			contexts: map[string]scm.Status{
				"broken-test": {
					Context: "broken-test",
					State:   scm.StatusPending,
				},
			},
			user:          "rando",
			checkComments: []string{"unauthorized"},
			expected: map[string]scm.Status{
				"broken-test": {
					Context: "broken-test",
					State:   scm.StatusPending,
				},
			},
		},
		{
			name:    "comment for override with no target",
			comment: "/override",
			contexts: map[string]scm.Status{
				"broken-test": {
					Context: "broken-test",
					State:   scm.StatusPending,
				},
			},
			user:          "rando",
			checkComments: []string{"but none was given"},
			expected: map[string]scm.Status{
				"broken-test": {
					Context: "broken-test",
					State:   scm.StatusPending,
				},
			},
		},
		{
			name:    "override multiple",
			comment: "/override broken-test\n/override hung-test",
			contexts: map[string]scm.Status{
				"broken-test": {
					Context: "broken-test",
					State:   scm.StatusFailure,
				},
				"hung-test": {
					Context: "hung-test",
					State:   scm.StatusPending,
				},
			},
			expected: map[string]scm.Status{
				"hung-test": {
					Context:     "hung-test",
					Description: description(adminUser),
					State:       scm.StateSuccess,
				},
				"broken-test": {
					Context:     "broken-test",
					Description: description(adminUser),
					State:       scm.StateSuccess,
				},
			},
			checkComments: []string{fmt.Sprintf("%s: broken-test, hung-test", adminUser)},
		},
		{
			name:    "ignore non-PRs",
			issue:   true,
			comment: "/override broken-test",
			contexts: map[string]scm.Status{
				"broken-test": {
					Context: "broken-test",
					State:   scm.StatusPending,
				},
			},
			expected: map[string]scm.Status{
				"broken-test": {
					Context: "broken-test",
					State:   scm.StatusPending,
				},
			},
		},
		{
			name:    "ignore closed issues",
			state:   "closed",
			comment: "/override broken-test",
			contexts: map[string]scm.Status{
				"broken-test": {
					Context: "broken-test",
					State:   scm.StatusPending,
				},
			},
			expected: map[string]scm.Status{
				"broken-test": {
					Context: "broken-test",
					State:   scm.StatusPending,
				},
			},
		},
		{
			name:    "ignore edits",
			action:  scm.ActionUpdate,
			comment: "/override broken-test",
			contexts: map[string]scm.Status{
				"broken-test": {
					Context: "broken-test",
					State:   scm.StatusPending,
				},
			},
			expected: map[string]scm.Status{
				"broken-test": {
					Context: "broken-test",
					State:   scm.StatusPending,
				},
			},
		},
		{
			name:    "ignore random text",
			comment: "/test broken-test",
			contexts: map[string]scm.Status{
				"broken-test": {
					Context: "broken-test",
					State:   scm.StatusPending,
				},
			},
			expected: map[string]scm.Status{
				"broken-test": {
					Context: "broken-test",
					State:   scm.StatusPending,
				},
			},
		},
		{
			name:    "comment on get pr failure",
			number:  fakePR * 2,
			comment: "/override broken-test",
			contexts: map[string]scm.Status{
				"broken-test": {
					Context: "broken-test",
					State:   scm.StatusFailure,
				},
			},
			expected: map[string]scm.Status{
				"broken-test": {
					Context:     "broken-test",
					Description: description(adminUser),
					State:       scm.StateSuccess,
				},
			},
			checkComments: []string{"Cannot get PR"},
		},
		{
			name:    "comment on list statuses failure",
			comment: "/override fail-list",
			contexts: map[string]scm.Status{
				"fail-list": {
					Context: "fail-list",
					State:   scm.StatusFailure,
				},
			},
			expected: map[string]scm.Status{
				"fail-list": {
					Context: "fail-list",
					State:   scm.StatusFailure,
				},
			},
			checkComments: []string{"Cannot get commit statuses"},
		},
		{
			name:    "do not override passing contexts",
			comment: "/override passing-test",
			contexts: map[string]scm.Status{
				"passing-test": {
					Context:     "passing-test",
					Description: "preserve description",
					State:       scm.StateSuccess,
				},
			},
			expected: map[string]scm.Status{
				"passing-test": {
					Context:     "passing-test",
					State:       scm.StateSuccess,
					Description: "preserve description",
				},
			},
		},
		{
			name:    "create successful prow job",
			comment: "/override prow-job",
			contexts: map[string]scm.Status{
				"prow-job": {
					Context:     "prow-job",
					Description: "failed",
					State:       scm.StatusFailure,
				},
			},
			presubmits: map[string]config.Presubmit{
				"prow-job": {
					Reporter: config.Reporter{
						Context: "prow-job",
					},
				},
			},
			jobs: sets.NewString("prow-job"),
			expected: map[string]scm.Status{
				"prow-job": {
					Context:     "prow-job",
					State:       scm.StateSuccess,
					Description: description(adminUser),
				},
			},
		},
		{
			name:    "override with explanation works",
			comment: "/override job\r\nobnoxious flake", // github ends lines with \r\n
			contexts: map[string]scm.Status{
				"job": {
					Context:     "job",
					Description: "failed",
					State:       scm.StatusFailure,
				},
			},
			expected: map[string]scm.Status{
				"job": {
					Context:     "job",
					Description: description(adminUser),
					State:       scm.StateSuccess,
				},
			},
		},
	}

	log := logrus.WithField("plugin", pluginName)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var event github.GenericCommentEvent
			event.Repo.Namespace = fakeOrg
			event.Repo.Name = fakeRepo
			event.Body = tc.comment
			event.Number = fakePR
			event.IsPR = !tc.issue
			if tc.user == "" {
				tc.user = adminUser
			}
			event.Author.Login = tc.user
			if tc.state == "" {
				tc.state = "open"
			}
			event.IssueState = tc.state
			if tc.action == "" {
				tc.action = scm.ActionCreate
			}
			event.Action = tc.action
			if tc.contexts == nil {
				tc.contexts = map[string]scm.Status{}
			}
			fc := fakeClient{
				statuses:   tc.contexts,
				presubmits: tc.presubmits,
				jobs:       sets.String{},
			}

			if tc.jobs == nil {
				tc.jobs = sets.String{}
			}

			err := handle(&fc, log, &event)
			switch {
			case err != nil:
				if !tc.err {
					t.Errorf("unexpected error: %v", err)
				}
			case tc.err:
				t.Error("failed to receive an error")
			case !reflect.DeepEqual(fc.statuses, tc.expected):
				t.Errorf("bad statuses: actual %#v != expected %#v", fc.statuses, tc.expected)
			case !reflect.DeepEqual(fc.jobs, tc.jobs):
				t.Errorf("bad jobs: actual %#v != expected %#v", fc.jobs, tc.jobs)
			}
		})
	}
}
