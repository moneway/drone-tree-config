package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/drone/drone-go/drone"
	"github.com/drone/drone-go/plugin/config"

	"github.com/google/go-github/github"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"gopkg.in/yaml.v2"
)

// New creates a drone plugin
func New(server, token string, concat bool, fallback bool) config.Plugin {
	return &plugin{
		server:   server,
		token:    token,
		concat:   concat,
		fallback: fallback,
	}
}

type (
	plugin struct {
		server   string
		token    string
		concat   bool
		fallback bool
	}

	droneConfig struct {
		Name string `yaml:"name"`
		Kind string `yaml:"kind"`
	}

	request struct {
		*config.Request
		UUID   uuid.UUID
		Client *github.Client
	}

	Config map[string]interface{}
)

func (cfg Config) getOrder() int {
	if raw, ok := cfg["order"]; ok {
		if value, ok := raw.(int); ok {
			return value
		} else {
			logrus.Error("The 'order' field must be an int")
		}
	}
	return 0
}

func (cfg Config) getName() string {
	if raw, ok := cfg["name"]; ok {
		if value, ok := raw.(string); ok {
			return value
		} else {
			logrus.Error("The 'name' field must be a string")
		}
	}
	return ""
}

func (cfg Config) addDependence(name string) {
	var deps []string
	if raw, ok := cfg["depends_on"]; ok {
		if value, ok := raw.([]string); ok {
			deps = value
		}
	}
	deps = append(deps, name)
	cfg["depends_on"] = deps
}

func (cfg Config) extractOrderDependences() []int {
	var raw interface{}
	var ok bool
	if raw, ok = cfg["depends_on_order"]; !ok {
		return []int{}
	}
	var values []interface{}
	if values, ok = raw.([]interface{}); !ok {
		logrus.Error("The 'depends_on_order' field must be an int array")
		return []int{}
	}
	deps := []int{}
	for _, value := range values {
		if realvalue, ok := value.(int); ok {
			deps = append(deps, realvalue)
		} else {
			logrus.Error("The 'depends_on_order' field must be an int array")
		}
	}
	delete(cfg, "depends_on_order")
	return deps
}

var dedupRegex = regexp.MustCompile(`(?ms)(---[\s]*){2,}`)

// MAIN
func (p *plugin) Find(ctx context.Context, droneRequest *config.Request) (*drone.Config, error) {
	uuid := uuid.New()
	logrus.Infof("%s %s/%s started", uuid, droneRequest.Repo.Namespace, droneRequest.Repo.Name)
	defer logrus.Infof("%s finished", uuid)

	// connect to github
	trans := oauth2.NewClient(ctx, oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: p.token},
	))
	var client *github.Client
	if p.server == "" {
		client = github.NewClient(trans)
	} else {
		var err error
		client, err = github.NewEnterpriseClient(p.server, p.server, trans)
		if err != nil {
			logrus.Errorf("%s Unable to connect to Github: '%v'", uuid, err)
			return nil, err
		}
	}

	req := request{droneRequest, uuid, client}

	// get changed files
	changedFiles, err := p.getGithubChanges(ctx, &req)
	if err != nil {
		return nil, err
	}

	// get drone.yml for changed files or all of them if no changes/cron
	configData := ""
	if changedFiles != nil {
		configData, err = p.getGithubConfigData(ctx, &req, changedFiles)
	} else if req.Build.Trigger == "@cron" {
		logrus.Warnf("%s @cron, rebuilding all", req.UUID)
		configData, err = p.getAllConfigData(ctx, &req, "/")
	} else if p.fallback {
		logrus.Warnf("%s no changed files and fallback enabled, rebuilding all", req.UUID)
		configData, err = p.getAllConfigData(ctx, &req, "/")
	}
	if err != nil {
		return nil, err
	}

	// no file found
	if configData == "" {
		return nil, errors.New("did not find a .drone.yml")
	}

	// cleanup
	configData = strings.ReplaceAll(configData, "...", "")
	configData = string(dedupRegex.ReplaceAll([]byte(configData), []byte("---")))

	return &drone.Config{Data: configData}, nil
}

// get repo changed files
func (p *plugin) getGithubChanges(ctx context.Context, req *request) ([]string, error) {
	var changedFiles []string

	if req.Build.Trigger == "@cron" {
		// cron jobs trigger a full build
		changedFiles = []string{}
	} else if strings.HasPrefix(req.Build.Ref, "refs/pull/") {
		// use pullrequests api to get changed files
		pullRequestID, err := strconv.Atoi(strings.Split(req.Build.Ref, "/")[2])
		if err != nil {
			logrus.Errorf("%s unable to get pull request id %v", req.UUID, err)
			return nil, err
		}
		opts := github.ListOptions{}
		files, _, err := req.Client.PullRequests.ListFiles(ctx, req.Repo.Namespace, req.Repo.Name, pullRequestID, &opts)
		if err != nil {
			logrus.Errorf("%s unable to fetch diff for Pull request %v", req.UUID, err)
			return nil, err
		}
		for _, file := range files {
			changedFiles = append(changedFiles, *file.Filename)
		}
	} else {
		// use diff to get changed files
		before := req.Build.Before
		if before == "0000000000000000000000000000000000000000" || before == "" {
			before = fmt.Sprintf("%s~1", req.Build.After)
		}
		changes, _, err := req.Client.Repositories.CompareCommits(ctx, req.Repo.Namespace, req.Repo.Name, before, req.Build.After)
		if err != nil {
			logrus.Errorf("%s unable to fetch diff: '%v'", req.UUID, err)
			return nil, err
		}
		for _, file := range changes.Files {
			changedFiles = append(changedFiles, *file.Filename)
		}
	}

	if len(changedFiles) > 0 {
		changedList := strings.Join(changedFiles, "\n  ")
		logrus.Debugf("%s changed files: \n  %s", req.UUID, changedList)
	} else {
		return nil, nil
	}
	return changedFiles, nil
}

// get the contents of a file on github, if the file is not found throw an error
func (p *plugin) getGithubFile(ctx context.Context, req *request, file string) (content string, err error) {
	logrus.Debugf("%s checking %s/%s %s", req.UUID, req.Repo.Namespace, req.Repo.Name, file)
	ref := github.RepositoryContentGetOptions{Ref: req.Build.After}
	data, _, _, err := req.Client.Repositories.GetContents(ctx, req.Repo.Namespace, req.Repo.Name, file, &ref)
	if data == nil {
		err = fmt.Errorf("failed to get %s: is not a file", file)
	}
	if err != nil {
		return "", err
	}
	return data.GetContent()
}

// download and validate a drone.yml
func (p *plugin) getGithubDroneConfig(ctx context.Context, req *request, file string) (configData string, critical bool, err error) {
	fileContent, err := p.getGithubFile(ctx, req, file)
	if err != nil {
		logrus.Debugf("%s skipping: unable to load file: %s %v", req.UUID, file, err)
		return "", false, err
	}

	// validate fileContent, exit early if an error was found
	dc := droneConfig{}
	err = yaml.Unmarshal([]byte(fileContent), &dc)
	if err != nil {
		logrus.Errorf("%s skipping: unable do parse yml file: %s %v", req.UUID, file, err)
		return "", true, err
	}
	if dc.Name == "" || dc.Kind == "" {
		logrus.Errorf("%s skipping: missing 'kind' or 'name' in %s.", req.UUID, file)
		return "", true, err
	}

	return fileContent, false, nil
}

// collect drone.yml files and return the full content concatenate
func (p *plugin) getGithubConfigData(ctx context.Context, req *request, changedFiles []string) (string, error) {
	cache := map[string]bool{}
	var configs []string
	for _, file := range changedFiles {
		if !strings.HasPrefix(file, "/") {
			file = "/" + file
		}

		done := false
		dir := file
		for !done {
			done = bool(dir == "/")
			dir = path.Join(dir, "..")
			file := path.Join(dir, req.Repo.Config)

			// check if file has already been checked
			_, ok := cache[file]
			if ok {
				continue
			} else {
				cache[file] = true
			}

			// download file from git
			fileContent, critical, err := p.getGithubDroneConfig(ctx, req, file)
			if err != nil {
				if critical {
					return "", err
				}
				continue
			}

			// append
			configs = append(configs, fileContent)
			logrus.Infof("%s found %s/%s %s", req.UUID, req.Repo.Namespace, req.Repo.Name, file)
			if !p.concat {
				logrus.Infof("%s concat is disabled. Using just first .drone.yml.", req.UUID)
				break
			}
		}
	}
	return p.droneConfigCreate(configs)
}

// search for all or fist drone.yml in repo
func (p *plugin) getAllConfigData(ctx context.Context, req *request, dir string) (string, error) {
	ref := github.RepositoryContentGetOptions{Ref: req.Build.After}
	_, ls, _, err := req.Client.Repositories.GetContents(ctx, req.Repo.Namespace, req.Repo.Name, dir, &ref)
	if err != nil {
		return "", err
	}

	// check recursivly for drone.yml
	var configs []string
	for _, f := range ls {
		var fileContent string
		if *f.Type == "dir" {
			fileContent, _ = p.getAllConfigData(ctx, req, *f.Path)
		} else if *f.Type == "file" && *f.Name == req.Repo.Config {
			var critical bool
			fileContent, critical, err = p.getGithubDroneConfig(ctx, req, *f.Path)
			if critical {
				return "", err
			}
		}
		// append
		configs = append(configs, fileContent)
		if !p.concat {
			logrus.Infof("%s concat is disabled. Using just first .drone.yml.", req.UUID)
			break
		}
	}
	return p.droneConfigCreate(configs)

}

func (p *plugin) mapCopy(dst, src interface{}) {
	dv, sv := reflect.ValueOf(dst), reflect.ValueOf(src)

	for _, k := range sv.MapKeys() {
		dv.SetMapIndex(k, sv.MapIndex(k))
	}
}

func (p *plugin) extractSubConfig(config string) (values []Config, err error) {
	dec := yaml.NewDecoder(strings.NewReader(config))
	var value Config
	for err = nil; err == nil; err = dec.Decode(&value) {
		if value != nil {
			newValue := Config{}
			p.mapCopy(newValue, value)
			values = append(values, newValue)
		}
		value = Config{}
	}
	if err == io.EOF {
		err = nil
	}
	return
}

func (p *plugin) substituteDepends(configs []Config) []Config {
	sortedConfigs := map[int][]Config{}
	for _, cfg := range configs {
		order := cfg.getOrder()
		sortedConfigs[order] = append(sortedConfigs[order], cfg)
	}
	for _, cfg := range configs {
		deps := cfg.extractOrderDependences()
		for _, dep := range deps {
			if values, ok := sortedConfigs[dep]; ok {
				for _, value := range values {
					cfg.addDependence(value.getName())
				}
			}
		}
	}
	return configs
}

func (p *plugin) droneConfigCreate(configs []string) (string, error) {
	var fullConfigs []Config
	for _, config := range configs {
		if subconfigs, err := p.extractSubConfig(config); err != nil {
			return "", err
		} else {
			fullConfigs = append(fullConfigs, subconfigs...)
		}
	}

	sort.SliceStable(fullConfigs, func(i, j int) bool {
		return fullConfigs[i].getOrder() < fullConfigs[j].getOrder()
	})

	fullConfigs = p.substituteDepends(fullConfigs)

	writer := bytes.NewBufferString("")
	encoder := yaml.NewEncoder(writer)
	fmt.Println("Pipelines : ")
	for _, m := range fullConfigs {
		fmt.Println(m.getName())
		if err := encoder.Encode(m); err != nil {
			return "", err
		}
	}
	newConfig := writer.String()
	fmt.Println("Full configs : ")
	fmt.Println(newConfig)
	return newConfig, nil
}
