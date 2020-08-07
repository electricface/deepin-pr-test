package main

import (
	"errors"
	"net/url"
	"regexp"

	gerrit "github.com/andygrunwald/go-gerrit"
)

func newGerritClient() (*gerrit.Client, error) {
	client, err := gerrit.NewClient("https://gerrit.uniontech.com", nil)
	return client, err
}

var regUrlSuccess = regexp.MustCompile(`(https://\S+) : SUCCESS`)

func getJobUrlFromGerritChange(client *gerrit.Client, changeID string) (string, *patchDetail, error) {
	changeInfo, _, err := client.Changes.GetChangeDetail(changeID, nil)
	if err != nil {
		return "", nil, err
	}

	detail := &patchDetail{
		id:    changeID,
		url:   changeInfo.URL,
		user:  changeInfo.Owner.Name,
		title: changeInfo.Subject,
		state: changeInfo.Status,
	}

	var jobUrl string
	for _, msg := range changeInfo.Messages {
		if msg.Author.Name != "jenkins" {
			continue
		}

		// author 都是 jenkins
		//"Patch Set 1: Verified+1 Code-Review+1\n\nBuild Successful \n\nhttps://jenkinswh.uniontech.com/job/gerrit-pipeline/1750/ : SUCCESS"
		match := regUrlSuccess.FindStringSubmatch(msg.Message)
		if match != nil {
			jobUrl0 := match[1]
			_, err := url.Parse(jobUrl0)
			if err == nil {
				jobUrl = jobUrl0
			}
		}
	}
	if jobUrl == "" {
		return "", nil, errors.New("not found job url")
	}
	return jobUrl, detail, nil
}
