package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"strings"

	log "github.com/Sirupsen/logrus"
	"gopkg.in/yaml.v2"
	core_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Handler interface contains the methods that are required
type Handler interface {
	Init(client kubernetes.Interface, config *rest.Config) error
	ObjectCreated(client kubernetes.Interface, obj interface{})
	ObjectDeleted(client kubernetes.Interface, obj interface{})
	ObjectUpdated(client kubernetes.Interface, objOld, objNew interface{})
}

// ResourceType represents the type of Kubernetes resource a pod belongs to.
type ResourceType byte

const (
	Unrecognized ResourceType = iota
	StatefulSet
	Deployment
)

// Action represents the action taken against a problematic pod.
type Action byte

const (
	Ignore Action = iota
	Scaledown
	Delete
)

// Policy encodes the policy structures in the config.yaml file.
type Policy struct {
	deployments  Action
	statefulSets Action
	whitelist []string
}

// HandlerImpl is a sample implementation of Handler
type HandlerImpl struct {
	clusterurl   string
	url          string
	user         string
	pass         string
	slackWebhook string
	webhookToken string
	unscanned    Policy
	security     Policy
	license      Policy
}

// NotifyComponentPayload is a component structure in NotifyPayload.
type NotifyComponentPayload struct {
	Name     string `json:"component_name"`
	Checksum string `json:"component_sha"`
}

// NotifyPayload is the payload used to notify xray of changes.
type NotifyPayload struct {
	Name       string                   `json:"pod_name"`
	Namespace  string                   `json:"namespace"`
	Action     string                   `json:"action"`
	Cluster    string                   `json:"cluster_url"`
	Components []NotifyComponentPayload `json:"components"`
}

// UnmarshalYAML is the unmarshaler implementation for the Policy type.
func (x *Policy) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var k map[string]interface{}
	err := unmarshal(&k)
	if err != nil {
		return err
	}
	deps, _ := k["deployments"].(string)
	switch deps {
	case "ignore":
		x.deployments = Ignore
	case "scaledown":
		x.deployments = Scaledown
	case "delete":
		x.deployments = Delete
	default:
		return errors.New("Cannot read action with value '" + deps + "'.")
	}
	sets, _ := k["statefulSets"].(string)
	switch sets {
	case "ignore":
		x.statefulSets = Ignore
	case "scaledown":
		x.statefulSets = Scaledown
	case "delete":
		x.statefulSets = Delete
	default:
		return errors.New("Cannot read action with value '" + sets + "'.")
	}
	whitelist := make([]string, 0)
	whitelists, _ := k["whitelistNamespaces"].([]interface{})
	for _, ns := range whitelists {
		namespace, ok := ns.(string)
		if ok {
			whitelist = append(whitelist, namespace)
		}
	}
	x.whitelist = whitelist
	return nil
}

// Init initializes the handler with configuration data.
func (t *HandlerImpl) Init(client kubernetes.Interface, config *rest.Config) error {
	log.Debug("HandlerImpl.Init")
	host := config.Host
	if host[len(host)-1] != '/' {
		host += "/"
	}
	t.clusterurl = host
	url, user, pass, slack, token, err := getXrayConfig("/config/secret/xray_config.yaml", "./xray_config.yaml")
	if err != nil {
		log.Error("Cannot read xray_config.yaml: ", err)
		return err
	}
	t.url = url
	t.user = user
	t.pass = pass
	t.slackWebhook = slack
	t.webhookToken = token
	unscanned, security, license, err := getConfig("/config/conf/config.yaml", "./config.yaml")
	if err != nil {
		log.Warn("Cannot read config.yaml: ", err)
	}
	t.unscanned = unscanned
	t.security = security
	t.license = license
	if t.webhookToken != "" {
		setupXrayWebhook(t, client)
	}
	return nil
}

// temporary structure for search results in webhook code
type searchItem struct {
	severity string
	isstype string
	sha2 string
	name string
	action string
	pod *core_v1.Pod
}

// parses the xray webhook request body
func parseWebhook(body interface{}) []searchItem {
	result := make([]searchItem, 0)
	bodymap := body.(map[string]interface{})
	for _, iss := range bodymap["issues"].([]interface{}) {
		issue := iss.(map[string]interface{})
		severity := issue["severity"].(string)
		isstype := issue["type"].(string)
		if (severity != "Major" && severity != "Critical" && severity != "High") || isstype == "" {
			continue
		}
		if _, ok := issue["impacted_artifacts"]; !ok {
			log.Debugf("Unable to process webhook, xray did not include impacted component data. Payload: %v", body)
			continue
		}
		for _, art := range issue["impacted_artifacts"].([]interface{}) {
			artif := art.(map[string]interface{})
			pkgtype := artif["pkg_type"].(string)
			sha2 := artif["sha256"].(string)
			if pkgtype != "Docker" || sha2 == "" {
				continue
			}
			res := searchItem{severity, isstype, sha2, "", "", nil}
			result = append(result, res)
		}
	}
	return result
}

// searches for checksums provided by the xray webhook, returning those that
// match active running containers
func searchChecksums(client kubernetes.Interface, shas []searchItem) ([]searchItem, error) {
	result := make([]searchItem, 0)
	nss, err := client.CoreV1().Namespaces().List(meta_v1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, ns := range nss.Items {
		pods, err := client.CoreV1().Pods(ns.Name).List(meta_v1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, pod := range pods.Items {
			for _, stat := range pod.Status.ContainerStatuses {
				idx := strings.LastIndex(stat.ImageID, "sha256:")
				if idx == -1 {
					continue
				}
				sha2 := stat.ImageID[idx+7:]
				for _, item := range shas {
					if item.sha2 == sha2 {
						res := item
						res.name = stat.Image
						res.pod = &pod
						result = append(result, res)
					}
				}
			}
		}
	}
	return result, nil
}

// setup the webhook for xray to call
func setupXrayWebhook(t *HandlerImpl, client kubernetes.Interface) {
	go func() {
		http.HandleFunc("/", handleXrayWebhook(t, client))
		err := http.ListenAndServe(":8765", nil)
		if err != nil {
			log.Errorf("Error running Xray webhook: %v", err)
		}
	}()
}

// handle when xray calls the webhook
func handleXrayWebhook(t *HandlerImpl, client kubernetes.Interface) http.HandlerFunc {
	return func(resp http.ResponseWriter, req *http.Request) {
		log.Debug("Webhook triggered by Xray")
		// check the auth token and fail if it's wrong
		toks := req.Header["X-Auth-Token"]
		if len(toks) <= 0 || toks[0] != t.webhookToken {
			log.Warn("Xray did not send an appropriate token, aborting webhook")
			resp.WriteHeader(403)
			return
		}
		// parse the webhook request payload
		body, err := ioutil.ReadAll(req.Body)
		if err != nil {
			log.Errorf("Error reading webhook request: %v", err)
			resp.WriteHeader(400)
			return
		}
		var data interface{}
		err = json.Unmarshal(body, &data)
		if err != nil {
			log.Errorf("Error reading webhook request: %v", err)
			resp.WriteHeader(400)
			return
		}
		// find matching checksums in the cluster
		searchterms := parseWebhook(data)
		searchresult, err := searchChecksums(client, searchterms)
		if err != nil {
			log.Errorf("Error handling webhook request: %v", err)
			resp.WriteHeader(500)
			return
		}
		// check each match against the config to decide how to deal with it
		for _, term := range searchresult {
			_, typ := checkResource(client, term.pod)
			if isWhitelistedNamespace(t, term.pod, true, term.isstype == "security", term.isstype == "license") {
				log.Debug("Ignoring pod: %s (due to whitelisted namespace: %s)", term.pod.Name, term.pod.Namespace)
				continue
			}
			delete, scaledown := false, false
			if typ == Deployment {
				if term.isstype == "security" {
					if t.security.deployments == Delete {
						delete = true
					} else if t.security.deployments == Scaledown {
						scaledown = true
					}
				} else if term.isstype == "license" {
					if t.license.deployments == Delete {
						delete = true
					} else if t.license.deployments == Scaledown {
						scaledown = true
					}
				}
			} else if typ == StatefulSet {
				if term.isstype == "security" {
					if t.security.statefulSets == Delete {
						delete = true
					} else if t.security.statefulSets == Scaledown {
						scaledown = true
					}
				} else if term.isstype == "license" {
					if t.license.statefulSets == Delete {
						delete = true
					} else if t.license.statefulSets == Scaledown {
						scaledown = true
					}
				}
			}
			if delete || scaledown {
				// remove the pod by either deleting it or scaling it to zero replicas
				if delete {
					term.action = "delete"
				} else {
					term.action = "scaledown"
				}
				removePod(client, term.pod, typ, delete)
			} else {
				log.Debugf("Ignoring pod: %s", term.pod.Name)
			}
		}
		// send notification to xray
		groups := make(map[types.UID][]*searchItem)
		for _, item := range searchresult {
			if item.action == "" {
				continue
			}
			group, ok := groups[item.pod.UID]
			if !ok {
				group = make([]*searchItem, 0)
			}
			groups[item.pod.UID] = append(group, &item)
		}
		for _, group := range groups {
			comp := make([]NotifyComponentPayload, 0)
			act := "scaledown"
			for _, item := range group {
				c := NotifyComponentPayload{Name: item.name, Checksum: item.sha2}
				if item.action == "delete" {
					act = "delete"
				}
				comp = append(comp, c)
			}
			payload := NotifyPayload{Name: group[0].pod.Name, Namespace: group[0].pod.Namespace, Action: act, Cluster: t.clusterurl, Components: comp}
			// send a slack notification if applicable
			if t.slackWebhook != "" {
				notifyForPod(t.slackWebhook, payload, group[0].isstype == "security", group[0].isstype == "license")
			}
			err := sendXrayNotify(t, payload)
			if err != nil {
				log.Errorf("Problem notifying xray about pod %s: %s", payload.Name, err)
			}
		}
		resp.WriteHeader(200)
	}
}

// ObjectCreated is called when an object is created
func (t *HandlerImpl) ObjectCreated(client kubernetes.Interface, obj interface{}) {
	pod := obj.(*core_v1.Pod)
	log.Debug("HandlerImpl.ObjectCreated")
	_, typ := checkResource(client, pod)
	comps, rec, seciss, liciss := getPodInfo(t, pod)
	if isWhitelistedNamespace(t, pod, rec, seciss, liciss) {
		log.Debug("Ignoring pod: %s (due to whitelisted namespace: %s)", pod.Name, pod.Namespace)
		return
	}
	delete, scaledown := false, false
	check := func(pol Policy) {
		if typ == Deployment && pol.deployments == Delete {
			delete = true
		} else if typ == Deployment && pol.deployments == Scaledown {
			scaledown = true
		} else if typ == StatefulSet && pol.statefulSets == Delete {
			delete = true
		} else if typ == StatefulSet && pol.statefulSets == Scaledown {
			scaledown = true
		}
	}
	if !rec {
		check(t.unscanned)
	}
	if seciss {
		check(t.security)
	}
	if liciss {
		check(t.license)
	}
	act := ""
	if delete {
		act = "delete"
	} else if scaledown {
		act = "scaledown"
	}
	payload := NotifyPayload{Name: pod.Name, Namespace: pod.Namespace, Action: act, Cluster: t.clusterurl, Components: comps}
	if t.slackWebhook != "" && (!rec || seciss || liciss) {
		notifyForPod(t.slackWebhook, payload, seciss, liciss)
	}
	if delete || scaledown {
		removePod(client, pod, typ, delete)
		err := sendXrayNotify(t, payload)
		if err != nil {
			log.Errorf("Problem notifying xray about pod %s: %s", payload.Name, err)
		}
	} else {
		log.Debugf("Ignoring pod: %s", pod.Name)
	}
}

// ObjectDeleted is called when an object is deleted
func (t *HandlerImpl) ObjectDeleted(client kubernetes.Interface, obj interface{}) {
	log.Debug("HandlerImpl.ObjectDeleted")
}

// ObjectUpdated is called when an object is updated
func (t *HandlerImpl) ObjectUpdated(client kubernetes.Interface, objOld, objNew interface{}) {
	log.Debug("HandlerImpl.ObjectUpdated")
}

// send the notification to xray
func sendXrayNotify(t *HandlerImpl, payload NotifyPayload) error {
	log.Debugf("Sending message back to xray concerning pod %s", payload.Name)
	client := &http.Client{}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	log.Debugf("Message body: %s", string(body))
	req, err := http.NewRequest("POST", t.url+"/api/v1/kube/metadata", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(t.user, t.pass)
	req.Header.Add("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("xray server responded with status: " + resp.Status)
	}
	return nil
}

// check if this namespace is in the whitelist for the provided violation type
func isWhitelistedNamespace(t *HandlerImpl, pod *core_v1.Pod, rec, seciss, liciss bool) bool {
	whitelist := make([]string, 0)
	if !rec {
		whitelist = append(whitelist, t.unscanned.whitelist...)
	}
	if seciss {
		whitelist = append(whitelist, t.security.whitelist...)
	}
	if liciss {
		whitelist = append(whitelist, t.license.whitelist...)
	}
	for _, ns := range whitelist {
		if ns == pod.Namespace {
			return true
		}
	}
	return false
}

// send a notification to slack
func notifyForPod(slack string, payload NotifyPayload, seciss, liciss bool) {
	log.Debugf("Sending notification concerning pod %s", payload.Name)
	if slack == "" {
		log.Warn("Unable to send notification, no Slack webhook URL configured")
		return
	}
	client := &http.Client{}
	msg1 := "*ignored*. "
	if payload.Action == "delete" {
		msg1 = "*deleted*. "
	} else if payload.Action == "scaledown" {
		msg1 = "*scaled to zero*. "
	}
	msg2 := "_Reason: Unrecognized by Xray_\n"
	if seciss {
		msg2 = "_Reason: Major security issue_\n"
	} else if liciss {
		msg2 = "_Reason: Major license issue_\n"
	}
	msg3 := "Affected components:"
	for _, comp := range payload.Components {
		msg3 += "\n• " + comp.Name + " _(sha256:" + comp.Checksum + ")_"
	}
	var js = map[string]string{
		"username": "kube-xray",
		"text":     "Pod *" + payload.Name + "* (in " + payload.Namespace + ") " + msg1 + msg2 + msg3,
	}
	encjs, err := json.Marshal(js)
	if err != nil {
		log.Warnf("Error notifying slack: %s", err)
		return
	}
	body := strings.NewReader(string(encjs))
	req, err := http.NewRequest("POST", slack, body)
	if err != nil {
		log.Warnf("Error notifying slack: %s", err)
		return
	}
	req.Header.Add("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		log.Warnf("Error notifying slack: %s", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Warnf("Error notifying slack: response code is %s", resp.Status)
		return
	}
	log.Debug("Notification successful")
}

// get the parent resource name and type of a given pod
func checkResource(client kubernetes.Interface, pod *core_v1.Pod) (string, ResourceType) {
	subs1 := strings.LastIndexByte(pod.Name, '-')
	if subs1 < 0 {
		log.Debugf("Resource for pod %s is not a recognized resource type", pod.Name)
		return "", Unrecognized
	}
	subs2 := strings.LastIndexByte(pod.Name[:subs1], '-')
	sets := client.AppsV1().StatefulSets(pod.Namespace)
	_, err := sets.Get(pod.Name[:subs1], meta_v1.GetOptions{})
	if err == nil {
		return pod.Name[:subs1], StatefulSet
	}
	log.Debugf("Resource for pod %s is not stateful set %s: %v", pod.Name, pod.Name[:subs1], err)
	if subs2 < 0 {
		log.Debugf("Resource for pod %s is not a recognized resource type", pod.Name)
		return "", Unrecognized
	}
	deps := client.AppsV1().Deployments(pod.Namespace)
	_, err = deps.Get(pod.Name[:subs2], meta_v1.GetOptions{})
	if err == nil {
		return pod.Name[:subs2], Deployment
	}
	log.Debugf("Resource for pod %s is not deployment %s: %v", pod.Name, pod.Name[:subs2], err)
	return "", Unrecognized
}

// remove a pod by either deleting it, or scaling it to zero replicas
func removePod(client kubernetes.Interface, pod *core_v1.Pod, typ ResourceType, delete bool) {
	deps := client.AppsV1().Deployments(pod.Namespace)
	sets := client.AppsV1().StatefulSets(pod.Namespace)
	subs1 := strings.LastIndexByte(pod.Name, '-')
	subs2 := strings.LastIndexByte(pod.Name[:subs1], '-')
	setname := pod.Name[:subs1]
	depname := pod.Name[:subs2]
	if delete && typ == StatefulSet {
		log.Infof("Deleting stateful set: %s", setname)
		err := sets.Delete(setname, &meta_v1.DeleteOptions{})
		if err != nil {
			log.Warnf("Cannot delete stateful set: %s", err)
		}
	} else if delete && typ == Deployment {
		log.Infof("Deleting deployment: %s", depname)
		err := deps.Delete(depname, &meta_v1.DeleteOptions{})
		if err != nil {
			log.Warnf("Cannot delete deployment: %s", err)
		}
	} else if !delete && typ == StatefulSet {
		log.Infof("Scaling stateful set to zero pods: %s", setname)
		set, err := sets.Get(setname, meta_v1.GetOptions{})
		if err != nil {
			log.Warnf("Cannot find stateful set: %s", err)
			return
		}
		*set.Spec.Replicas = 0
		_, err = sets.Update(set)
		if err != nil {
			log.Warnf("Cannot update stateful set: %s", err)
		}
	} else if !delete && typ == Deployment {
		log.Infof("Scaling deployment to zero pods: %s", depname)
		dep, err := deps.Get(depname, meta_v1.GetOptions{})
		if err != nil {
			log.Warnf("Cannot find deployment: %s", err)
			return
		}
		*dep.Spec.Replicas = 0
		_, err = deps.Update(dep)
		if err != nil {
			log.Warnf("Cannot update deployment: %s", err)
		}
	} else {
		log.Warnf("Unable to handle case: delete = %v, type = %v", delete, typ)
	}
}

// check a new pod against xray and extract useful information about it
func getPodInfo(t *HandlerImpl, pod *core_v1.Pod) ([]NotifyComponentPayload, bool, bool, bool) {
	components := make([]NotifyComponentPayload, 0)
	recognized := true
	hassecissue := false
	haslicissue := false
	log.Debugf("Pod: %s v.%s (Node: %s, %s)", pod.Name, pod.ObjectMeta.ResourceVersion,
		pod.Spec.NodeName, pod.Status.Phase)
	for _, status := range pod.Status.ContainerStatuses {
		idx := strings.LastIndex(status.ImageID, "sha256:")
		var sha2 string
		if idx != -1 {
			sha2 = status.ImageID[idx+7:]
		} else {
			sha2 = "NA"
		}
		log.Debugf("Container: %s, Digest: %s", status.Image, sha2)
		if sha2 != "NA" && t.url != "" {
			rec, secissue, licissue, err := checkXray(sha2, t.url, t.user, t.pass)
			if err == nil {
				comp := NotifyComponentPayload{Name: status.Image, Checksum: sha2}
				components = append(components, comp)
				recognized = recognized && rec
				hassecissue = hassecissue || secissue
				haslicissue = haslicissue || licissue
			}
		}
	}
	return components, recognized, hassecissue, haslicissue
}

// parse the config.yaml file and return its contents
func getConfig(path, path2 string) (Policy, Policy, Policy, error) {
	file, err := ioutil.ReadFile(path)
	if err != nil {
		file, err = ioutil.ReadFile(path2)
		if err != nil {
			return Policy{}, Policy{}, Policy{}, err
		}
	}
	var data map[string]Policy
	err = yaml.Unmarshal([]byte(file), &data)
	if err != nil {
		return Policy{}, Policy{}, Policy{}, err
	}
	return data["unscanned"], data["security"], data["license"], nil
}

// parse the xray_config.yaml file and return its contents
func getXrayConfig(path, path2 string) (string, string, string, string, string, error) {
	file, err := ioutil.ReadFile(path)
	if err != nil {
		file, err = ioutil.ReadFile(path2)
		if err != nil {
			return "", "", "", "", "", err
		}
	}
	var data map[string]string
	err = yaml.Unmarshal([]byte(file), &data)
	if err != nil {
		return "", "", "", "", "", err
	}
	url, urlok := data["url"]
	user, userok := data["user"]
	pass, passok := data["password"]
	if urlok && userok && passok {
		return url, user, pass, data["slackWebhookUrl"], data["xrayWebhookToken"], nil
	}
	return "", "", "", "", "", errors.New("xray_config.yaml does not contain required information")
}

// ComponentPayload is the component structure in ComponentAPIResponse, as well
// as the request payload for the xray violation API.
type ComponentPayload struct {
	Package string `json:"package_id"`
	Version string `json:"version"`
}

// ComponentAPIResponse is the response from the xray component API.
type ComponentAPIResponse struct {
	Checksum   string             `json:"sha256"`
	Components []ComponentPayload `json:"ids"`
}

// ViolationAPIResponseItem is the item structure in a ViolationAPIResponse.
type ViolationAPIResponseItem struct {
	Type     string `json:"type"`
	Severity string `json:"severity"`
}

// ViolationAPIResponse is the response from the xray violation API.
type ViolationAPIResponse struct {
	Total int                        `json:"total_count"`
	Data  []ViolationAPIResponseItem `json:"data"`
}

// ask xray about the checksums in a given pod, specifically for any violations
func checkXray(sha2, url, user, pass string) (bool, bool, bool, error) {
	apiNotFound := errors.New("404 response, try the backup API instead")
	log.Debugf("Checking sha %s with Xray ...", sha2)
	var data ComponentAPIResponse
	err := func(data *ComponentAPIResponse) error {
		client := &http.Client{}
		req, err := http.NewRequest("GET", url+"/api/v1/componentIdsByChecksum/"+sha2, nil)
		if err != nil {
			log.Warnf("Error checking xray: %s", err)
			return err
		}
		req.SetBasicAuth(user, pass)
		resp, err := client.Do(req)
		if err != nil {
			log.Warnf("Error checking xray: %s", err)
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode == 404 {
			return apiNotFound
		}
		if resp.StatusCode != 200 {
			log.Warnf("Error checking xray: response code is %s", resp.Status)
			return errors.New("xray server responded with status: " + resp.Status)
		}
		err = json.NewDecoder(resp.Body).Decode(data)
		if err != nil {
			log.Warnf("Error checking xray: %s", err)
			return err
		}
		return nil
	}(&data)
	if err == apiNotFound {
		log.Debug("404 response from componentIdsByChecksum, trying backup API instead")
		return checkXrayBackup(sha2, url, user, pass)
	}
	if err != nil {
		return false, false, false, err
	}
	if len(data.Components) <= 0 {
		log.Debug("Xray does not recognize this sha")
		return false, false, false, nil
	}
	for _, comp := range data.Components {
		bodyjson, err := json.Marshal(&comp)
		if err != nil {
			log.Warnf("Error checking xray: %s", err)
			return false, false, false, err
		}
		var resp ViolationAPIResponse
		err = func(data *ViolationAPIResponse) error {
			client := &http.Client{}
			path := "/ui/userIssues/details?direction=asc&order_by=severity&num_of_rows=0&page_num=0"
			body := bytes.NewReader(bodyjson)
			req, err := http.NewRequest("POST", url+path, body)
			if err != nil {
				log.Warnf("Error checking xray: %s", err)
				return err
			}
			req.SetBasicAuth(user, pass)
			req.Header.Add("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				log.Warnf("Error checking xray: %s", err)
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				log.Warnf("Error checking xray: response code is %s", resp.Status)
				return errors.New("xray server responded with status: " + resp.Status)
			}
			err = json.NewDecoder(resp.Body).Decode(data)
			if err != nil {
				log.Warnf("Error checking xray: %s", err)
				return err
			}
			return nil
		}(&resp)
		if err != nil {
			return false, false, false, err
		}
		for _, item := range resp.Data {
			if item.Severity == "High" {
				if item.Type == "security" {
					log.Infof("Major security violation found for sha: %s", sha2)
					return true, true, false, nil
				} else if item.Type == "licenses" || item.Type == "license" {
					log.Infof("Major license violation found for sha: %s", sha2)
					return true, false, true, nil
				}
			}
		}
	}
	log.Debug("No major security issues found")
	return true, false, false, nil
}

// ask xray about the checksums in a given pod, specifically for any issues
func checkXrayBackup(sha2, url, user, pass string) (bool, bool, bool, error) {
	log.Debugf("Checking sha %s with Xray ...", sha2)
	client := &http.Client{}
	body := strings.NewReader("{\"checksums\":[\"" + sha2 + "\"]}")
	req, err := http.NewRequest("POST", url+"/api/v1/summary/artifact", body)
	if err != nil {
		log.Warnf("Error checking xray: %s", err)
		return false, false, false, err
	}
	req.SetBasicAuth(user, pass)
	req.Header.Add("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		log.Warnf("Error checking xray: %s", err)
		return false, false, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Warnf("Error checking xray: response code is %s", resp.Status)
		return false, false, false, errors.New("xray server responded with status: " + resp.Status)
	}
	var data interface{}
	json.NewDecoder(resp.Body).Decode(&data)
	dt := data.(map[string]interface{})
	artifacts := dt["artifacts"].([]interface{})
	if len(artifacts) <= 0 {
		log.Debug("Xray does not recognize this sha")
		return false, false, false, nil
	}
	for _, artifact := range artifacts {
		art := artifact.(map[string]interface{})
		issues := art["issues"].([]interface{})
		for _, issue := range issues {
			is := issue.(map[string]interface{})
			typ := is["issue_type"].(string)
			sev := is["severity"].(string)
			if typ == "security" && (sev == "Major" || sev == "Critical" || sev == "High") {
				log.Infof("Major security issue found for sha: %s", sha2)
				return true, true, false, nil
			}
			if typ == "license" && (sev == "Major" || sev == "Critical" || sev == "High") {
				log.Infof("Major license issue found for sha: %s", sha2)
				return true, false, true, nil
			}
		}
	}
	log.Debug("No major security issues found")
	return true, false, false, nil
}
