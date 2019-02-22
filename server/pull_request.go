// Copyright (c) 2017-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package server

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/mattermost/mattermost-mattermod/model"
	"github.com/mattermost/mattermost-server/mlog"
)

func handlePullRequestEvent(event *PullRequestEvent) {
	pr, err := GetPullRequestFromGithub(event.PullRequest)
	if err != nil {
		LogError(err.Error())
		return
	}

	if event.Action == "closed" {
		if result := <-Srv.Store.Spinmint().Get(pr.Number); result.Err != nil {
			LogError(fmt.Sprintf("Unable to get the spinmint information: %v. Maybe does not exist.", result.Err.Error()), mlog.String("err", result.Err.Error()))
		} else if result.Data == nil {
			LogInfo("Nothing to do. There is not Spinmint for this PR %v", pr.Number)
		} else {
			spinmint := result.Data.(*model.Spinmint)
			LogInfo("Spinmint instance %v", spinmint.InstanceId)
			LogInfo("Will destroy the spinmint for a merged/closed PR.")

			commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.DestroyedSpinmintMessage)
			go destroySpinmint(pr, spinmint.InstanceId)
		}
	}

	checkPullRequestForChanges(pr)
}

func checkPullRequestForChanges(pr *model.PullRequest) {
	var oldPr *model.PullRequest
	if result := <-Srv.Store.PullRequest().Get(pr.RepoOwner, pr.RepoName, pr.Number); result.Err != nil {
		LogError(result.Err.Error())
		return
	} else if result.Data == nil {
		if result := <-Srv.Store.PullRequest().Save(pr); result.Err != nil {
			LogError(result.Err.Error())
		}

		handlePROpened(pr)

		for _, label := range pr.Labels {
			handlePRLabeled(pr, label)
		}
		return
	} else {
		oldPr = result.Data.(*model.PullRequest)
	}

	prHasChanges := false

	for _, label := range pr.Labels {
		hadLabel := false

		for _, oldLabel := range oldPr.Labels {
			if label == oldLabel {
				hadLabel = true
				break
			}
		}

		if !hadLabel {
			handlePRLabeled(pr, label)
			prHasChanges = true
		}
	}

	for _, oldLabel := range oldPr.Labels {
		hasLabel := false

		for _, label := range pr.Labels {
			if label == oldLabel {
				hasLabel = true
				break
			}
		}

		if !hasLabel {
			handlePRUnlabeled(pr, oldLabel)
			prHasChanges = true
		}
	}

	if oldPr.Ref != pr.Ref {
		prHasChanges = true
	}

	if oldPr.Sha != pr.Sha {
		prHasChanges = true
	}

	if oldPr.BuildStatus != pr.BuildStatus {
		prHasChanges = true
	}

	if oldPr.BuildLink != pr.BuildLink {
		prHasChanges = true
	}

	if prHasChanges {
		LogInfo(fmt.Sprintf("pr %v has changes", pr.Number))
		if result := <-Srv.Store.PullRequest().Save(pr); result.Err != nil {
			LogError(result.Err.Error())
			return
		}
	}
}

func handlePROpened(pr *model.PullRequest) {
	username := pr.Username

	resp, err := http.Get(Config.SignedCLAURL)
	if err != nil {
		LogError("Unable to get CLA list: " + err.Error())
		return
	}

	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		LogError("Unable to read response body: " + err.Error())
		return
	}

	if !strings.Contains(string(body), ">"+username+"<") {
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, strings.Replace(Config.NeedsToSignCLAMessage, "USERNAME", "@"+username, 1))
	}
}

func handlePRLabeled(pr *model.PullRequest, addedLabel string) {
	LogInfo(fmt.Sprintf("labeled pr %v with %v", pr.Number, addedLabel))

	// Must be sure the comment is created before we let anouther request test
	commentLock.Lock()
	defer commentLock.Unlock()

	comments, _, err := NewGithubClient().Issues.ListComments(pr.RepoOwner, pr.RepoName, pr.Number, nil)
	if err != nil {
		LogError(fmt.Sprintf("Unable to list comments for PR %v: %v", pr.Number, err.Error()))
		return
	}

	if addedLabel == Config.SetupSpinmintTag && !messageByUserContains(comments, Config.Username, Config.SetupSpinmintMessage) {
		LogInfo("Label to spin a test server")
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.SetupSpinmintMessage)
		go waitForBuildAndSetupSpinmint(pr, false)
	} else if addedLabel == Config.SetupSpinmintUpgradeTag && !messageByUserContains(comments, Config.Username, Config.SetupSpinmintUpgradeMessage) {
		LogInfo("Label to spin a test server for upgrade")
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.SetupSpinmintUpgradeMessage)
		go waitForBuildAndSetupSpinmint(pr, true)
	} else if addedLabel == Config.BuildMobileAppTag && !messageByUserContains(comments, Config.Username, Config.BuildMobileAppInitMessage) {
		LogInfo("Label to build the mobile app")
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.BuildMobileAppInitMessage)
		go waitForMobileAppsBuild(pr)
	} else if addedLabel == Config.StartLoadtestTag {
		LogInfo("Label to spin a load test")
		commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.StartLoadtestMessage)
		go waitForBuildAndSetupLoadtest(pr)
	} else {
		LogInfo("looking for other labels")

		for _, label := range Config.PrLabels {
			LogInfo("looking for " + label.Label)
			finalMessage := strings.Replace(label.Message, "USERNAME", pr.Username, -1)
			if label.Label == addedLabel && !messageByUserContains(comments, Config.Username, finalMessage) {
				LogInfo("Posted message for label: " + label.Label + " on PR: " + strconv.Itoa(pr.Number))
				commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, finalMessage)
			}
		}
	}
}

func checkFileExists(filepath string) bool {
	if _, err := os.Stat(filepath); err == nil {
		return true
	}
	return false
}

func handlePRUnlabeled(pr *model.PullRequest, removedLabel string) {
	commentLock.Lock()
	defer commentLock.Unlock()

	comments, _, err := NewGithubClient().Issues.ListComments(pr.RepoOwner, pr.RepoName, pr.Number, nil)
	if err != nil {
		LogError(err.Error())
		return
	}

	if (removedLabel == Config.SetupSpinmintTag || removedLabel == Config.SetupSpinmintUpgradeTag) &&
		(messageByUserContains(comments, Config.Username, Config.SetupSpinmintMessage) || messageByUserContains(comments, Config.Username, Config.SetupSpinmintUpgradeMessage)) &&
		!messageByUserContains(comments, Config.Username, Config.DestroyedSpinmintMessage) {

		upgrade := false
		if removedLabel == Config.SetupSpinmintUpgradeTag {
			upgrade = true
		}

		var instanceId string
		for _, comment := range comments {
			if isSpinmintDoneComment(*comment.Body, upgrade) {
				match := INSTANCE_ID_PATTERN.FindStringSubmatch(*comment.Body)
				instanceId = match[1]
				break
			}
		}

		if instanceId != "" {
			commentOnIssue(pr.RepoOwner, pr.RepoName, pr.Number, Config.DestroyedSpinmintMessage)

			go destroySpinmint(pr, instanceId)
		}
	}
}

func isSpinmintDoneComment(message string, upgrade bool) bool {
	var spinmintDoneMessage string
	if upgrade {
		spinmintDoneMessage = regexp.QuoteMeta(Config.SetupSpinmintUpgradeDoneMessage)
	} else {
		spinmintDoneMessage = regexp.QuoteMeta(Config.SetupSpinmintDoneMessage)
	}
	spinmintDoneMessage = strings.Replace(spinmintDoneMessage, SPINMINT_LINK, ".*", -1)
	spinmintDoneMessage = strings.Replace(spinmintDoneMessage, INSTANCE_ID, INSTANCE_ID_PATTERN.String(), -1)

	pattern := regexp.MustCompile(spinmintDoneMessage)
	return pattern.MatchString(message)
}
