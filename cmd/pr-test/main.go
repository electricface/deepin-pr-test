package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codeskyblue/go-sh"
	"github.com/google/go-github/github"
	"github.com/levigross/grequests"
	"golang.org/x/oauth2"
	"pault.ag/go/debian/control"
	"pault.ag/go/debian/dependency"
)

var VERSION = "unknown"

var flagStatus bool
var flagVerbose bool
var flagRestore string
var flagVersion bool
var flagUpgradeSelf bool

func init() {
	flag.BoolVar(&flagStatus, "status", false, "")
	flag.BoolVar(&flagVerbose, "verbose", false, "")
	flag.BoolVar(&flagVersion, "version", false, "")
	flag.BoolVar(&flagUpgradeSelf, "upgrade", false, "")
	flag.StringVar(&flagRestore, "restore", "", "all|$repo|$user")
}

const (
	organization = "linuxdeepin"

	tempDebDownloadDir = "/tmp/pr-test/deb_download"
	tempDebModifiedDir = "/tmp/pr-test/deb_modified"
)

func getUrlBasename(u *url.URL) (string, error) {
	p, err := url.PathUnescape(u.Path)
	if err != nil {
		return "", err
	}
	base := path.Base(p)
	return base, nil
}

func saveDeb(debUrl *url.URL, detail *jobDetail) (modifiedFilename string, err error) {
	u := debUrl.String()
	debug("download from", u)
	resp, err := grequests.Get(u, nil)
	if err != nil {
		return
	}

	base, err := getUrlBasename(debUrl)
	if err != nil {
		return
	}
	filename := filepath.Join(tempDebDownloadDir, base)

	err = os.MkdirAll(tempDebDownloadDir, 0755)
	if err != nil {
		return
	}

	err = resp.DownloadToFile(filename)
	if err != nil {
		return
	}

	modifiedFilename, err = modifyDeb(filename, &debDetail{
		url:       u,
		jobDetail: detail,
	})
	return
}

func modifyDeb(filename string, detail *debDetail) (modifiedFilename string, err error) {
	modifiedFilename = filepath.Join(tempDebModifiedDir, filepath.Base(filename))
	debug("modifiedFilename:", modifiedFilename)

	err = os.MkdirAll(tempDebModifiedDir, 0755)
	if err != nil {
		return
	}

	err = sh.Command("cp", filename, modifiedFilename).Run()
	if err != nil {
		return
	}

	tempDir, err := ioutil.TempDir("", "pr-test-mod")
	if err != nil {
		return
	}
	debug("tempDir:", tempDir)
	defer func() {
		err := os.RemoveAll(tempDir)
		if err != nil {
			log.Println("WARN:", err)
		}
	}()

	arFiles, err := getArFiles(modifiedFilename)
	if err != nil {
		return
	}

	var controlTarFile string
	var tarExt string
	for _, file := range arFiles {
		if strings.HasPrefix(file, "control.tar") {
			controlTarFile = file
			tarExt = filepath.Ext(file)
		}
	}

	if controlTarFile == "" {
		err = errors.New("not found control tar file in deb file")
		return
	}

	session := sh.NewSession().SetDir(tempDir)
	err = session.Command("ar", "x", modifiedFilename, controlTarFile).Run()
	if err != nil {
		return
	}

	const (
		extGz = ".gz"
		extXz = ".xz"
	)
	switch tarExt {
	case extGz:
		err = session.Command("gunzip", controlTarFile).Run()
	case extXz:
		err = session.Command("xz", "-d", controlTarFile).Run()
	default:
		err = fmt.Errorf("unknown control.tar ext %q", tarExt)
	}
	if err != nil {
		return
	}

	err = session.Command("tar", "--extract", "--file=control.tar", "./control").Run()
	if err != nil {
		return
	}

	err = modifyControl(filepath.Join(tempDir, "control"), detail)
	if err != nil {
		return
	}

	// rebuild deb
	err = session.Command("tar", "--update", "-f", "control.tar", "./control").Run()
	if err != nil {
		return
	}

	switch tarExt {
	case extGz:
		err = session.Command("gzip", "control.tar").Run()
	case extXz:
		err = session.Command("xz", "-z", "control.tar").Run()
	}
	if err != nil {
		return
	}

	err = session.Command("ar", "r", modifiedFilename, controlTarFile).Run()
	if err != nil {
		return
	}

	return
}

func replaceDependsVersion(d dependency.Dependency, oldVer, newVer string) string {
	for _, r := range d.Relations {
		for pIdx := range r.Possibilities {
			ver := r.Possibilities[pIdx].Version
			if ver != nil && ver.Number == oldVer {
				r.Possibilities[pIdx].Version.Number = newVer
			}
		}
	}
	return d.String()
}

func modifyControl(filename string, detail *debDetail) error {
	fh, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer func() {
		err := fh.Close()
		if err != nil {
			log.Println("WARN: failed to close file:", err)
		}
	}()
	br := bufio.NewReader(fh)

	var binParagraph control.BinaryParagraph
	err = control.Unmarshal(&binParagraph, br)
	if err != nil {
		return err
	}

	pkgName := binParagraph.Package
	oldVer := binParagraph.Values["Version"]
	oldDepends := binParagraph.Values["Depends"]
	newVer, err := getNewVersion(pkgName)
	if err != nil {
		log.Printf("WARN: failed to get new version for %s: %v\n", pkgName, err)
	}
	if newVer != "" {
		binParagraph.Set("Version", newVer)
		binParagraph.Set("Depends", replaceDependsVersion(binParagraph.Depends, oldVer, newVer))
	}

	var descBuf bytes.Buffer
	descBuf.WriteString(binParagraph.Description)
	descBuf.WriteString("The following information is added by deepin-pr-test\n=begin\n")
	prDetail := detail.jobDetail.prDetail
	parts := []string{
		"DEPENDS", oldDepends,
		"PR_URL", prDetail.url,
		"PR_REPO", prDetail.repo,
		"PR_NUM", strconv.Itoa(prDetail.num),
		"PR_USER", prDetail.user,
		"PR_TITLE", prDetail.title,
		"PR_STATE", prDetail.state,

		"CI_URL", detail.jobDetail.url,

		"DEB_URL", detail.url,
		"DEB_MODIFY_TIME", time.Now().Format(time.RFC3339),
	}
	for i := 0; i < len(parts); i += 2 {
		descBuf.WriteString(parts[i])
		descBuf.WriteByte('=')
		descBuf.WriteString(parts[i+1])
		descBuf.WriteByte('\n')
	}
	// NOTE: paragraph 的 Description 不能以 \n 结尾。
	descBuf.WriteString("=end")
	binParagraph.Set("Description", descBuf.String())

	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer func() {
		err = f.Close()
		if err != nil {
			log.Println(err)
		}
	}()

	bw := bufio.NewWriter(f)
	err = binParagraph.WriteTo(bw)
	if err != nil {
		return err
	}
	err = bw.Flush()
	if err != nil {
		return err
	}

	if flagVerbose {
		err = binParagraph.WriteTo(os.Stdout)
		if err != nil {
			return err
		}
	}

	return nil
}

var globalClient *github.Client

func getGithubClient() *github.Client {
	if globalClient != nil {
		return globalClient
	}

	token, err := getGithubAccessToken()
	if err != nil {
		log.Println("WARN: failed to get github access token:", err)
	}

	ctx := context.Background()
	var httpClient *http.Client
	if token != "" {
		httpClient = oauth2.NewClient(ctx, oauth2.StaticTokenSource(
			&oauth2.Token{
				AccessToken: token,
			}))
	}
	client := github.NewClient(httpClient)
	globalClient = client
	return client
}

func main() {
	log.SetFlags(log.Lshortfile | log.LstdFlags)
	log.SetOutput(os.Stdout)
	flag.Parse()

	if flagStatus {
		err := showStatus()
		if err != nil {
			log.Fatal(err)
		}
		return
	} else if flagRestore != "" {
		err := restore(flagRestore)
		if err != nil {
			log.Fatal(err)
		}
		return
	} else if flagVersion {
		fmt.Println(VERSION)
		return
	} else if flagUpgradeSelf {
		err := upgradeSelf()
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	client := getGithubClient()
	var prIds []pullRequestId
	for _, arg := range flag.Args() {
		ids, err := getPrIdsFromCmdArg(client, arg)
		if err != nil {
			log.Fatal(err)
		}
		prIds = append(prIds, ids...)
	}
	prIds = uniqPrIds(prIds)
	prIdsStrList := make([]string, len(prIds))
	for idx, id := range prIds {
		prIdsStrList[idx] = id.String()
	}
	debug("found pull request:", strings.Join(prIdsStrList, ", "))
	for _, prId := range prIds {
		err := installPullRequest(client, prId)
		if err != nil {
			log.Fatal(err)
		}
	}
}

func getPrIdsFromCmdArg(client *github.Client, arg string) ([]pullRequestId, error) {
	issueShortReg := regexp.MustCompile(`^@([^#]+)#(\d+)$`)
	// match @xxx#37
	match := issueShortReg.FindStringSubmatch(arg)
	if match != nil {
		repo := match[1]
		num, err := strconv.Atoi(match[2])
		if err != nil {
			return nil, err
		}
		switch repo {
		case "id":
			repo = "internal-discussion"
		case "dc":
			repo = "developer-center"
		}
		arg1 := fmt.Sprintf("https://github.com/%s/%s/issues/%d", organization, repo, num)
		arg = arg1
	}

	if strings.Contains(arg, "/issues/") {
		return getPrIdsWithIssue(client, arg)
	}
	id, err := getPRIdFromCmdArg(arg)
	if err != nil {
		return nil, err
	}
	return []pullRequestId{id}, nil
}

var globalPRCache = make(map[pullRequestId]*github.PullRequest)

func getPullRequest(client *github.Client, repo string, num int) (*github.PullRequest, error) {
	pr := globalPRCache[pullRequestId{repo: repo, num: num}]
	if pr != nil {
		return pr, nil
	}

	ctx := context.Background()
	pr, _, err := client.PullRequests.Get(ctx, organization, repo, num)
	if err != nil {
		return nil, err
	}

	globalPRCache[pullRequestId{repo: repo, num: num}] = pr
	return pr, nil
}

func getPrIdsWithIssue(client *github.Client, issueUrl string) ([]pullRequestId, error) {
	iId, err := parseIssueUrl(issueUrl)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	page := 1
	var prIds []pullRequestId
	for {
		timeline, resp, err := client.Issues.ListIssueTimeline(ctx, iId.owner, iId.repo, iId.num, &github.ListOptions{
			Page:    page,
			PerPage: 100,
		})
		if err != nil {
			return nil, err
		}

		for _, timelineItem := range timeline {
			if timelineItem.GetEvent() == "cross-referenced" {
				src := timelineItem.GetSource()
				if src.GetIssue() != nil &&
					src.GetIssue().IsPullRequest() {
					prLinks := src.GetIssue().GetPullRequestLinks()
					prId, err := parsePullUrl(prLinks.GetHTMLURL())
					if err != nil {
						continue
					}

					pr, err := getPullRequest(client, prId.repo, prId.num)
					if err != nil {
						return nil, err
					}
					if pr.GetState() == "closed" && !pr.GetMerged() {
						debug("ignore abandoned:", prId.String())
						continue
					}
					prIds = append(prIds, prId)
				}
			}
		}

		if resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	return prIds, nil
}

type pullRequestId struct {
	repo string
	num  int
}

func (prId pullRequestId) String() string {
	return fmt.Sprintf("%s#%d", prId.repo, prId.num)
}

func uniqPrIds(ids []pullRequestId) (result []pullRequestId) {
	repoNumsMap := make(map[string][]int)
	for _, id := range ids {
		repoNumsMap[id.repo] = append(repoNumsMap[id.repo], id.num)
	}

	for repo, nums := range repoNumsMap {
		sort.Ints(nums)
		num := nums[len(nums)-1]
		result = append(result, pullRequestId{repo: repo, num: num})
	}
	return
}

func getPRIdFromCmdArg(arg string) (pullRequestId, error) {
	num, err := strconv.Atoi(arg)
	if err == nil {
		repo, err := getRepoFromGitConfig()
		if err != nil {
			return pullRequestId{}, err
		}
		return pullRequestId{repo: repo, num: num}, nil
	}
	reg := regexp.MustCompile(`^(\S+)#(\d+)$`)
	match := reg.FindStringSubmatch(arg)
	if match != nil {
		repo := match[1]
		num, err := strconv.Atoi(match[2])
		if err != nil {
			return pullRequestId{}, err
		}
		return pullRequestId{repo: repo, num: num}, nil
	}

	return parsePullUrl(arg)
}

func getRepoFromGitConfig() (string, error) {
	out, err := sh.Command("git", "config", "--local",
		"--get-regexp", `remote\..*\.url`).Output()
	if err != nil {
		return "", err
	}
	remotes := bytes.Split(out, []byte("\n"))
	reg := regexp.MustCompile(fmt.Sprintf(`github.com[:/]%s/(.+)$`, organization))
	for _, remote := range remotes {
		match := reg.FindSubmatch(remote)
		if match != nil {
			repo := string(match[1])
			repo = strings.TrimSuffix(repo, ".git")
			return repo, nil
		}
	}
	return "", errors.New("repo not found in remote urls")
}

func getSuccessStatus(statuses []*github.RepoStatus) *github.RepoStatus {
	for _, status := range statuses {
		if status.GetState() == "success" {
			return status
		}
	}
	return nil
}

func parsePullUrl(pullUrl string) (prId pullRequestId, err error) {
	reg := regexp.MustCompile("https://github.com/" + organization + `/([^/]+)/pull/(\d+)`)
	match := reg.FindStringSubmatch(pullUrl)
	if match == nil {
		err = errors.New("invalid pull url")
		return
	}

	prId.repo = match[1]
	prId.num, err = strconv.Atoi(match[2])
	return
}

type issueId struct {
	owner string
	repo  string
	num   int
}

func parseIssueUrl(issueUrl string) (iId issueId, err error) {
	reg := regexp.MustCompile(`https://github.com/([^/]+)/([^/]+)/issues/(\d+)`)
	match := reg.FindStringSubmatch(issueUrl)
	if match == nil {
		err = errors.New("invalid issue url")
		return
	}

	iId.owner = match[1]
	iId.repo = match[2]
	iId.num, err = strconv.Atoi(match[3])
	return
}

var regHrefDeb1 = regexp.MustCompile(`href="(\S+\.deb)">`)
var regHrefDeb2 = regexp.MustCompile(`\.href = '(\S+\.deb)'`)

func getDebUrls(jobUrl string) ([]*url.URL, error) {
	resp, err := grequests.Get(jobUrl, nil)
	if err != nil {
		return nil, err
	}

	if !strings.HasSuffix(jobUrl, "/") {
		jobUrl += "/"
	}

	respStr := resp.String()
	allMatch := regHrefDeb1.FindAllStringSubmatch(respStr, -1)
	if allMatch == nil {
		// deb urls are collapsed, try another regex
		allMatch = regHrefDeb2.FindAllStringSubmatch(respStr, -1)
	}

	if allMatch == nil {
		return nil, nil
	}

	var result = make([]*url.URL, len(allMatch))
	for idx, match := range allMatch {
		u, err := url.Parse(jobUrl + match[1])
		if err != nil {
			return nil, err
		}
		result[idx] = u
	}
	return result, nil
}

func askYesNo(prompt string, defaultYes bool) (yes bool, err error) {
	var suffix string
	if defaultYes {
		suffix = " (Yes/n) "
	} else {
		suffix = " (y/No) "
	}
	fmt.Print(prompt, suffix)
	var input string
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		input = scanner.Text()
		input = strings.TrimSpace(input)
	}
	if scanner.Err() != nil {
		return false, scanner.Err()
	}
	if input == "" {
		return defaultYes, nil
	}
	return strings.HasPrefix(input, "y") ||
		strings.HasPrefix(input, "Y"), nil
}

func parseDebFilename(filename string) (pkgName, version, arch string, err error) {
	filename = strings.TrimSuffix(filename, ".deb")
	fields := strings.SplitN(filename, "_", 3)
	if len(fields) != 3 {
		err = errors.New("parseDebFilename: len fields != 3")
		return
	}
	pkgName = fields[0]
	version = fields[1]
	arch = fields[2]
	return
}

func showPullRequestInfo(prId pullRequestId, pr *github.PullRequest) {
	fmt.Printf("> %s #%d\n", prId.repo, prId.num)
	fmt.Println("title:", pr.GetTitle())
	fmt.Println("state:", pr.GetState())
	fmt.Println("merged:", pr.GetMerged())
	fmt.Println("user:", pr.GetUser().GetLogin())
}

type debDetail struct {
	url       string
	jobDetail *jobDetail
}

type jobDetail struct {
	url      string
	prDetail *pullRequestDetail
}

type pullRequestDetail struct {
	pullRequestId
	url   string
	user  string
	title string
	state string
}

func installPullRequest(client *github.Client, prId pullRequestId) error {
	ctx := context.Background()
	pr, err := getPullRequest(client, prId.repo, prId.num)
	if err != nil {
		return err
	}

	showPullRequestInfo(prId, pr)

	prRef := pr.GetHead().GetSHA()
	if prRef == "" {
		return errors.New("failed to get pull request ref")
	}
	statuses, _, err := client.Repositories.ListStatuses(ctx, organization, prId.repo,
		prRef, nil)
	if err != nil {
		return err
	}

	status := getSuccessStatus(statuses)
	if status == nil {
		var targetUrl0 string
		for _, status := range statuses {
			targetUrl := status.GetTargetURL()
			if targetUrl != "" {
				targetUrl0 = targetUrl
			}
			break
		}

		errMsg := "not found success status"
		if targetUrl0 != "" {
			errMsg += ", please see " + targetUrl0
		}
		return errors.New(errMsg)
	}

	targetUrl := status.GetTargetURL()
	if targetUrl == "" {
		return errors.New("target url is empty")
	}

	debug("targetUrl:", targetUrl)

	err = installJobDebs(strings.TrimSuffix(targetUrl, "/console"), pr)
	return err
}

func needDefaultInstall(pkgName string) bool {
	if strings.HasSuffix(pkgName, "-dev") ||
		strings.HasSuffix(pkgName, "-dbg") ||
		strings.HasSuffix(pkgName, "-dbgsym") {
		return false
	}

	switch pkgName {
	case "libdtkwidget-bin":
		return false
	}

	return true
}

func installJobDebs(jobUrl string, pr *github.PullRequest) error {
	debUrls, err := getDebUrls(jobUrl)
	if err != nil {
		return err
	}

	pkgUrlMap := make(map[string]*url.URL)
	for _, debUrl := range debUrls {
		base, err := getUrlBasename(debUrl)
		if err != nil {
			return err
		}

		pkgName, _, _, err := parseDebFilename(base)
		if err != nil {
			return err
		}

		defaultYes := needDefaultInstall(pkgName)
		respYes, err := askYesNo(fmt.Sprintf("install %s?", pkgName), defaultYes)
		if err != nil {
			return err
		}

		if respYes {
			pkgUrlMap[pkgName] = debUrl
		}
	}

	if len(pkgUrlMap) == 0 {
		return nil
	}

	var files []string
	for _, debUrl := range pkgUrlMap {
		prDetail := new(pullRequestDetail)
		prDetail.num = pr.GetNumber()
		prDetail.repo = pr.GetBase().GetRepo().GetName()
		prDetail.url = pr.GetHTMLURL()
		prDetail.user = pr.GetUser().GetLogin()
		prDetail.title = pr.GetTitle()
		prDetail.state = pr.GetState()
		jobDetail := &jobDetail{
			url:      jobUrl,
			prDetail: prDetail,
		}
		var filename string
		filename, err = saveDeb(debUrl, jobDetail)
		if err != nil {
			return err
		}
		files = append(files, filename)
	}

	// simulate
	commonCmdArgs := []string{"apt-get", "install", "-y",
		"--allow-downgrades", "--reinstall"}
	cmdArgs := append(commonCmdArgs, "-s")
	cmdArgs = append(cmdArgs, files...)
	err = sh.Command("sudo", cmdArgs).Run()
	if err != nil {
		log.Println("WARN: simulate install failed:", err)
		return err
	}

	replyYes, err := askYesNo("Do you want to continue?", true)
	if err != nil {
		return err
	}
	if !replyYes {
		return nil
	}

	for pkgName := range pkgUrlMap {
		err = markInstall(pkgName)
		if err != nil {
			return err
		}
	}

	cmdArgs = append(commonCmdArgs, files...)
	err = sh.Command("sudo", cmdArgs).Run()
	return err
}

func showStatus() error {
	all, _, err := getAllPkgInstallDetails()
	if err != nil {
		return err
	}

	for _, detail := range all {
		fmt.Println("Repo:", detail["PR_REPO"])
		fmt.Println("Package:", detail["pkgs"])
		fmt.Println("Title:", detail["PR_TITLE"])
		fmt.Println("User:", detail["PR_USER"])
		fmt.Println("PR url:", detail["PR_URL"])
		fmt.Println("Job url:", detail["CI_URL"], "\n")
	}
	return nil
}

func getAllPkgInstallDetails() (allDetails map[string]map[string]string, invalidList []string, err error) {
	fileInfos, err := ioutil.ReadDir(markDir)
	if err != nil {
		return
	}
	allDetails = make(map[string]map[string]string)
	for _, fileInfo := range fileInfos {
		pkg := fileInfo.Name()
		var detail map[string]string
		detail, err = getPkgInstallDetail(pkg)
		if err != nil {
			log.Println("WARN:", err)
			err = nil
		}
		if len(detail) == 0 {
			debugF("detail about %s is empty\n", pkg)
			invalidList = append(invalidList, pkg)
			continue
		}

		key := detail["CI_URL"]
		if key == "" {
			continue
		}
		v, ok := allDetails[key]
		if ok {
			v["pkgs"] = v["pkgs"] + " " + pkg
		} else {
			detail["pkgs"] = pkg
			allDetails[key] = detail
		}
	}
	return
}

func getPkgInstallDetail(pkg string) (detail map[string]string, err error) {
	out, err := sh.Command("dpkg-query", "-f", `${db:Status-Status}\n${Description}\n`, "--show", pkg).CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 1 {
				err = nil
				return
			}
		}
		return
	}

	lines := bytes.Split(out, []byte{'\n'})
	status := string(lines[0])
	if status != "installed" {
		return
	}

	var begin bool
	detail = make(map[string]string, 9)
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if !begin {
			if bytes.Equal(line, []byte("=begin")) {
				begin = true
			}
			continue
		} else {
			if bytes.Equal(line, []byte("=end")) {
				break
			}

			fields := bytes.SplitN(line, []byte{'='}, 2)
			if len(fields) != 2 {
				continue
			}

			key := string(fields[0])
			value := string(fields[1])
			detail[key] = value
		}
	}
	return
}

func restore(pattern string) error {
	allDetail, invalidList, err := getAllPkgInstallDetails()
	if err != nil {
		return err
	}

	var pkgList []string
	for _, detail := range allDetail {
		if pattern == "all" ||
			detail["PR_REPO"] == pattern ||
			detail["PR_USER"] == pattern {

			pkgListTemp := strings.Fields(detail["pkgs"])
			pkgList = append(pkgList, pkgListTemp...)
		}
	}
	debug("pkgList:", pkgList)
	debug("invalidList:", invalidList)

	if len(pkgList) == 0 && len(invalidList) == 0 {
		return nil
	}

	if len(pkgList) > 0 {
		fmt.Println("restore", pkgList)

		cmdArgs := []string{"apt-get", "install", "--fix-missing", "-y", "--reinstall"}
		cmdArgs = append(cmdArgs, pkgList...)
		err = sh.Command("sudo", cmdArgs).Run()
		if err != nil {
			return err
		}
	}

	for _, pkg := range append(pkgList, invalidList...) {
		detail, err := getPkgInstallDetail(pkg)
		if err != nil {
			log.Println("WARN:", err)
		}

		if len(detail) == 0 {
			// restore success
			err = markUninstall(pkg)
			if err != nil {
				return err
			}
		} else {
			log.Println("WARN: failed to restore", pkg)
		}
	}
	return err
}

func upgradeSelf() error {
	const (
		owner = "electricface"
		repo  = "deepin-pr-test"
	)

	client := getGithubClient()
	ctx := context.Background()
	release, _, err := client.Repositories.GetReleaseByTag(ctx, owner, repo, "latest")
	if err != nil {
		return err
	}

	fmt.Println("latest release:", release.GetBody())
	if strings.Contains(release.GetBody(), "version: "+VERSION) {
		fmt.Println("already the latest version")
		return nil
	}

	scriptUrl := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/master/scripts/install.sh",
		owner, repo)
	resp, err := grequests.Get(scriptUrl, nil)
	if err != nil {
		return err
	}
	scriptFilename := filepath.Join(os.TempDir(), "deepin-pr-test-install.sh")
	debug("download install script from:", scriptUrl)
	err = resp.DownloadToFile(scriptFilename)
	if err != nil {
		return err
	}
	err = os.Chmod(scriptFilename, 0777)
	if err != nil {
		return err
	}
	tempDir, err := ioutil.TempDir("", "deepin-pr-test-install")
	if err != nil {
		return err
	}
	defer func() {
		err := os.RemoveAll(tempDir)
		if err != nil {
			log.Println("WARN:", err)
		}
	}()

	session := sh.NewSession().SetDir(tempDir)
	err = session.Command(scriptFilename).Run()
	return err
}

func getNewVersion(pkgName string) (string, error) {
	out, err := sh.Command("env", "LC_ALL=C", "apt-cache", "policy", pkgName).Output()
	if err != nil {
		return "", err
	}
	/*
		输出类似于

		bash:
		  Installed: 4.4.18-2+b1
		  Candidate: 4.4.18-2+b1
	*/
	lines := bytes.Split(out, []byte{'\n'})

	getVal := func(line []byte) string {
		fields := bytes.Fields(bytes.TrimSpace(line))
		if len(fields) < 2 {
			return ""
		}
		return string(fields[1])
	}

	installedVer := getVal(lines[1])
	candidateVer := getVal(lines[2])

	if candidateVer != "" && installedVer == "(none)" {
		return candidateVer, nil
	}

	return installedVer, nil
}
