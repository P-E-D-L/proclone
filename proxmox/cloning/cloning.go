package cloning

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/P-E-D-L/proclone/auth"
	"github.com/P-E-D-L/proclone/proxmox"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

type CloneRequest struct {
	TemplateName string `json:"template_name" binding:"required"`
}

type NewPoolResponse struct {
	Success int `json:"success,omitempty"`
}

type CloneResponse struct {
	Success int      `json:"success"`
	PodName string   `json:"pod_name"`
	Errors  []string `json:"errors,omitempty"`
}

/*
 * ===== CLONE VMS FROM TEMPLATE POOL TO POD POOL =====
 */
func CloneTemplateToPod(c *gin.Context) {
	session := sessions.Default(c)
	username := session.Get("username")

	// Make sure user is authenticated
	isAuth, _ := auth.IsAuthenticated(c)
	if !isAuth {
		log.Printf("Unauthorized access attempt")
		c.JSON(http.StatusForbidden, gin.H{
			"error": "Only authenticated users can access pod data",
		})
		return
	}

	// Parse request body
	var req CloneRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Invalid request format",
			"details": err.Error(),
		})
		return
	}

	templatePool := "kamino_template_" + req.TemplateName

	// Load Proxmox configuration
	config, err := proxmox.LoadProxmoxConfig()
	if err != nil {
		log.Printf("Configuration error for user %s: %v", username, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to load Proxmox configuration: %v", err),
		})
		return
	}

	// Get all virtual resources
	apiResp, err := proxmox.GetVirtualResources(config)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to fetch virtual resources",
			"details": err.Error(),
		})
		return
	}

	// Find VMs in template pool
	var templateVMs []proxmox.VirtualResource
	for _, r := range *apiResp {
		if r.Type == "qemu" && r.ResourcePool == templatePool {
			templateVMs = append(templateVMs, r)
		}
	}

	if len(templateVMs) == 0 {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("No VMs found in template pool: %s", templatePool),
		})
		return
	}

	NewPodID, err := nextPodID(config, c)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to get a pod ID",
			"details": err.Error(),
		})
		return
	}

	NewPodPool, err := createNewPodPool(username.(string), NewPodID, req.TemplateName, config)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to create new pod resource pool",
			"details": err.Error(),
		})
		return
	}

	// Clone each VM
	var errors []string
	for _, vm := range templateVMs {
		err := cloneVM(config, vm, NewPodPool)
		if err != nil {
			errors = append(errors, fmt.Sprintf("Failed to clone VM %s: %v", vm.Name, err))
		}
	}

	err = setPoolPermission(config, NewPodPool, username.(string))
	if err != nil {
		errors = append(errors, fmt.Sprintf("Failed to update pool permissions for %s: %v", username, err))
	}

	var success int = 0
	if len(errors) == 0 {
		success = 1
	}

	response := CloneResponse{
		Success: success,
		PodName: NewPodPool,
		Errors:  errors,
	}

	if len(errors) > 0 {
		// if an error has occured, count # of successfully cloned VMs
		var clonedVMs []proxmox.VirtualResource
		for _, r := range *apiResp {
			if r.Type == "qemu" && r.ResourcePool == NewPodPool {
				clonedVMs = append(templateVMs, r)
			}
		}

		// if there are no cloned VMs in the resource pool, clean up the resource pool
		if len(clonedVMs) == 0 {
			cleanupFailedPodPool(config, NewPodPool)
		}

		// send response :)
		c.JSON(http.StatusPartialContent, response)
	} else {
		c.JSON(http.StatusOK, response)
	}
}

// assign a user to be a VM user for a resource pool
func setPoolPermission(config *proxmox.ProxmoxConfig, pool string, user string) error {

	// Create HTTP client with SSL verification based on config
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !config.VerifySSL},
	}
	client := &http.Client{Transport: tr}

	// define proxmox pools endpoint URL
	accessURL := fmt.Sprintf("https://%s:%s/api2/json/access/acl", config.Host, config.Port)

	// define json data holding new pool name
	jsonString := fmt.Sprintf("{\"path\":\"/pool/%s\", \"users\":\"%s@SDC\", \"roles\":\"PVEVMUser,PVEPoolUser\", \"propagate\": true }", pool, user)
	jsonData := []byte(jsonString)

	// Create request
	req, err := http.NewRequest("PUT", accessURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s", config.APIToken))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// Make request
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to assign pool permissions for %s: %v", user, err)
	}
	defer resp.Body.Close()

	return nil
}

func cleanupFailedClone(config *proxmox.ProxmoxConfig, nodeName string, vmid int) error {
	// Create HTTP client with SSL verification based on config
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !config.VerifySSL},
	}
	client := &http.Client{Transport: tr}

	// Prepare delete URL
	deleteURL := fmt.Sprintf("https://%s:%s/api2/json/nodes/%s/qemu/%d",
		config.Host, config.Port, nodeName, vmid)

	// Create request
	req, err := http.NewRequest("DELETE", deleteURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create cleanup request: %v", err)
	}

	// Add headers
	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s", config.APIToken))

	// Make request
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to cleanup VM: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to cleanup VM: %s", string(body))
	}

	return nil
}

func cloneVM(config *proxmox.ProxmoxConfig, vm proxmox.VirtualResource, newPool string) error {
	// Create a single HTTP client for all requests
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !config.VerifySSL},
	}
	client := &http.Client{Transport: tr}

	// Get next available VMID
	nextIDURL := fmt.Sprintf("https://%s:%s/api2/json/cluster/nextid", config.Host, config.Port)
	req, err := http.NewRequest("GET", nextIDURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create VMID request: %v", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s", config.APIToken))

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to get next VMID: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to get next VMID: %s", string(body))
	}

	var nextIDResponse struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&nextIDResponse); err != nil {
		return fmt.Errorf("failed to decode VMID response: %v", err)
	}

	newVMID, err := strconv.Atoi(nextIDResponse.Data)
	if err != nil {
		return fmt.Errorf("invalid VMID received: %v", err)
	}

	// Prepare and execute clone request
	cloneURL := fmt.Sprintf("https://%s:%s/api2/json/nodes/%s/qemu/%d/clone",
		config.Host, config.Port, vm.NodeName, vm.VmId)

	body := map[string]interface{}{
		"newid": newVMID,
		"name":  fmt.Sprintf("%s-clone", vm.Name),
		"pool":  newPool,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to create request body: %v", err)
	}

	req, err = http.NewRequest("POST", cloneURL, bytes.NewBuffer(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create clone request: %v", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s", config.APIToken))
	req.Header.Set("Content-Type", "application/json")

	resp, err = client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to clone VM: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to clone VM: %s", string(body))
	}

	// Wait for clone completion with exponential backoff
	statusURL := fmt.Sprintf("https://%s:%s/api2/json/nodes/%s/qemu/%d/status/current",
		config.Host, config.Port, vm.NodeName, newVMID)

	backoff := time.Second
	maxBackoff := 30 * time.Second
	timeout := 5 * time.Minute
	startTime := time.Now()

	for {
		if time.Since(startTime) > timeout {
			if err := cleanupFailedClone(config, vm.NodeName, newVMID); err != nil {
				return fmt.Errorf("clone timed out and cleanup failed: %v", err)
			}
			return fmt.Errorf("clone operation timed out after %v", timeout)
		}

		req, err = http.NewRequest("GET", statusURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create status check request: %v", err)
		}
		req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s", config.APIToken))

		resp, err = client.Do(req)
		if err != nil {
			return fmt.Errorf("failed to check clone status: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			// Verify the VM is actually cloned
			var statusResponse struct {
				Data struct {
					Status string `json:"status"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&statusResponse); err != nil {
				return fmt.Errorf("failed to decode status response: %v", err)
			}
			if statusResponse.Data.Status == "running" || statusResponse.Data.Status == "stopped" {
				lockURL := fmt.Sprintf("https://%s:%s/api2/json/nodes/%s/qemu/%d/config",
					config.Host, config.Port, vm.NodeName, newVMID)
				lockReq, err := http.NewRequest("GET", lockURL, nil)
				if err != nil {
					return fmt.Errorf("failed to create lock check request: %v", err)
				}
				lockReq.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s", config.APIToken))

				lockResp, err := client.Do(lockReq)
				if err != nil {
					return fmt.Errorf("failed to check lock status: %v", err)
				}
				defer lockResp.Body.Close()

				var configResp struct {
					Data struct {
						Lock string `json:"lock"`
					} `json:"data"`
				}
				if err := json.NewDecoder(lockResp.Body).Decode(&configResp); err != nil {
					return fmt.Errorf("failed to decode lock status: %v", err)
				}
				if configResp.Data.Lock == "" {
					return nil // Clone is complete and VM is not locked
				}
			}
		}

		time.Sleep(backoff)
		backoff = time.Duration(math.Min(float64(backoff*2), float64(maxBackoff)))
	}
}

// finds lowest available POD ID between 1001 - 1255
func nextPodID(config *proxmox.ProxmoxConfig, c *gin.Context) (string, error) {
	podResponse, err := getPodResponse(config)

	// if error, return error status
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to fetch pod list from proxmox cluster",
			"details": err,
		})
		return "", err
	}

	pods := podResponse.Pods
	var ids []int

	// for each pod name, get id from name and append to int array
	for _, pod := range pods {
		id, _ := strconv.Atoi(pod.Name[:4])
		ids = append(ids, id)
	}

	sort.Ints(ids)

	var nextId int
	var gapFound bool = false

	// find first id available starting from 1001
	for i := 1001; i <= 1000+len(ids); i++ {
		nextId = i
		if ids[i-1001] != i {
			gapFound = true
			break
		}
	}

	if !gapFound {
		nextId = 1001 + len(ids)
	}

	// if no ids available between 0 - 255 return error
	if nextId == 1256 {
		err = fmt.Errorf("no pod ids available")
		return "", err
	}

	return strconv.Itoa(nextId), nil
}

func cleanupFailedPodPool(config *proxmox.ProxmoxConfig, poolName string) error {
	// Create HTTP client with SSL verification based on config
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !config.VerifySSL},
	}
	client := &http.Client{Transport: tr}

	// Prepare delete URL
	// define proxmox pools endpoint URL
	poolDeleteURL := fmt.Sprintf("https://%s:%s/api2/json/pools/%s", config.Host, config.Port, poolName)

	// Create request
	req, err := http.NewRequest("DELETE", poolDeleteURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create pool cleanup request: %v", err)
	}

	// Add headers
	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s", config.APIToken))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// Make request
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to cleanup pool: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to cleanup pool: %s", string(body))
	}

	return nil
}

func createNewPodPool(username string, newPodID string, templateName string, config *proxmox.ProxmoxConfig) (string, error) {
	newPoolName := newPodID + "_" + templateName + "_" + username

	// Create HTTP client with SSL verification based on config
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !config.VerifySSL},
	}
	client := &http.Client{Transport: tr}

	// define proxmox pools endpoint URL
	poolURL := fmt.Sprintf("https://%s:%s/api2/extjs/pools", config.Host, config.Port)

	// define json data holding new pool name
	jsonString := fmt.Sprintf("{\"poolid\":\"%s\"}", newPoolName)
	jsonData := []byte(jsonString)

	// Create request
	req, err := http.NewRequest("POST", poolURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s", config.APIToken))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// Make request
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to create new pool: %v", err)
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read VM shutdown response: %v", err)
	}

	// Parse response
	var newPoolResponse NewPoolResponse
	if err := json.Unmarshal(body, &newPoolResponse); err != nil {
		return "", fmt.Errorf("failed to parse new pool response: %v", err)
	}

	return newPoolName, nil
}
