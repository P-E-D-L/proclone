package proxmox

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

const CRITICAL_POOL string = "0030_Critical"

type VMResponse struct {
	Data []VirtualResource `json:"data"`
}
type VirtualResource struct {
	CPU           float64 `json:"cpu,omitempty"`
	MaxCPU        int     `json:"maxcpu,omitempty"`
	Mem           int     `json:"mem,omitempty"`
	MaxMem        int     `json:"maxmem,omitempty"`
	Type          string  `json:"type,omitempty"`
	Id            string  `json:"id,omitempty"`
	Name          string  `json:"name,omitempty"`
	NodeName      string  `json:"node,omitempty"`
	ResourcePool  string  `json:"pool,omitempty"`
	RunningStatus string  `json:"status,omitempty"`
	Uptime        int     `json:"uptime,omitempty"`
	VmId          int     `json:"vmid,omitempty"`
	Storage       string  `json:"storage,omitempty"`
	Disk          int64   `json:"disk,omitempty"`
	MaxDisk       int64   `json:"maxdisk,omitempty"`
	Template      int     `json:"template,omitempty"`
}

type VirtualMachineResponse struct {
	VirtualMachines     []VirtualResource `json:"virtual_machines"`
	VirtualMachineCount int               `json:"virtual_machine_count"`
	RunningCount        int               `json:"running_count"`
}

type VM struct {
	VMID int    `json:"vmid" binding:"required"`
	Node string `json:"node" binding:"required"`
}

type VMPower struct {
	Success int    `json:"success"`
	Data    string `json:"data"`
}

type VMPowerResponse struct {
	Success int `json:"success"`
}

type PoolResponse struct {
	Data []Pool `json:"data"`
}

type Pool struct {
	Poolid  string            `json:"poolid"`
	Members []VirtualResource `json:"members"`
}

/*
 * ===== GET ALL VIRTUAL MACHINES =====
 */
func GetVirtualMachines(c *gin.Context) {
	session := sessions.Default(c)
	username := session.Get("username")
	isAdmin := session.Get("is_admin")

	// Make sure user is admin (redundant with middleware)
	if !isAdmin.(bool) {
		log.Printf("Unauthorized access attempt by user %s", username)
		c.JSON(http.StatusForbidden, gin.H{
			"error": "Only admin users can access vm data",
		})
		return
	}

	// store proxmox config
	var config *ProxmoxConfig
	var err error
	config, err = LoadProxmoxConfig()
	if err != nil {
		log.Printf("Configuration error for user %s: %v", username, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to load Proxmox configuration: %v", err),
		})
		return
	}

	// If no proxmox host specified, return empty repsonse
	if config.Host == "" {
		log.Printf("No proxmox server configured")
		c.JSON(http.StatusOK, VirtualMachineResponse{VirtualMachines: []VirtualResource{}})
		return
	}

	// fetch all virtual machines
	var virtualMachines *[]VirtualResource
	var error error
	var response VirtualMachineResponse = VirtualMachineResponse{}
	response.RunningCount = 0

	// get virtual machine info and include in response
	virtualMachines, error = GetVirtualMachineResponse(config)

	// if error, return error status
	if error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to fetch vm list from proxmox cluster",
			"details": error,
		})
		return
	}

	response.VirtualMachines = *virtualMachines

	// get total # of virtual machines and include in response
	response.VirtualMachineCount = len(*virtualMachines)

	// get # of running virtual machines and include in response
	for _, vm := range *virtualMachines {
		if vm.RunningStatus == "running" {
			response.RunningCount++
		}
	}

	log.Printf("Successfully fetched vm list for user %s", username)
	c.JSON(http.StatusOK, response)

}

// handles fetching all the virtual machines on the proxmox cluster
func GetVirtualResources(config *ProxmoxConfig) (*[]VirtualResource, error) {
	// Create HTTP client with SSL verification based on config
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !config.VerifySSL},
	}
	client := &http.Client{Transport: tr}

	vmURL := fmt.Sprintf("https://%s:%s/api2/json/cluster/resources", config.Host, config.Port)

	// Create request
	req, err := http.NewRequest("GET", vmURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	// Add API token header
	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s", config.APIToken))

	// Send request
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get proxmox resources: %v", err)
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read proxmox resource response: %v", err)
	}

	// Parse response into VMResponse struct
	var apiResp VMResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse status response: %v", err)
	}

	return &apiResp.Data, nil

}

func GetVirtualMachineResponse(config *ProxmoxConfig) (*[]VirtualResource, error) {

	// get all virtual resources from proxmox
	apiResp, err := GetVirtualResources(config)

	// if error, return error
	if err != nil {
		return nil, err
	}

	// Extract virtual machines from response, store in VirtualMachine struct array
	var vms []VirtualResource
	for _, r := range *apiResp {
		// don't return VMS in critical resource pool, for security
		if r.Type == "qemu" && r.ResourcePool != CRITICAL_POOL {
			vms = append(vms, r)
		}
	}

	return &vms, nil
}

/*
 * ====== POWERING OFF VIRTUAL MACHINES ======
 * POST requires "vmid" and "node" fields
 */
func PowerOffVirtualMachine(c *gin.Context) {
	session := sessions.Default(c)
	username := session.Get("username")
	isAdmin := session.Get("is_admin")

	// Make sure user is admin (redundant with middleware)
	if !isAdmin.(bool) {
		log.Printf("Unauthorized access attempt by user %s", username)
		c.JSON(http.StatusForbidden, gin.H{
			"error": "Only admin users can access vm data",
		})
		return
	}

	// store proxmox config
	var config *ProxmoxConfig
	var err error
	config, err = LoadProxmoxConfig()
	if err != nil {
		log.Printf("Configuration error for user %s: %v", username, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to load Proxmox configuration: %v", err),
		})
		return
	}

	// If no proxmox host specified, return empty repsonse
	if config.Host == "" {
		log.Printf("No proxmox server configured")
		c.JSON(http.StatusOK, VirtualMachineResponse{VirtualMachines: []VirtualResource{}})
		return
	}

	// If no nodes specified, return empty response
	if len(config.Nodes) == 0 {
		log.Printf("No nodes configured for user %s", username)
		c.JSON(http.StatusOK, ResourceUsageResponse{Nodes: []NodeResourceUsage{}})
		return
	}

	// get req.VMID, req.Node
	var req VM
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: must include 'vmid' and 'node'"})
		return
	}

	// log request on backend
	log.Printf("User %s requested to power off VM %d on node %s", username, req.VMID, req.Node)

	var error error
	var response *VMPower

	response, error = PowerOffRequest(config, req)

	// If we have error , return error status
	if error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "Failed to fetch resource usage for any nodes",
			"details": error,
		})
		return
	}

	var finalResponse VMPowerResponse
	finalResponse.Success = response.Success

	if finalResponse.Success == 1 {
		log.Printf("Successfully powered down VMID %s for %s", strconv.Itoa(req.VMID), username)
		c.JSON(http.StatusOK, response)
	} else {
		log.Printf("Failed to power down VMID %s for %s", strconv.Itoa(req.VMID), username)
		c.JSON(http.StatusOK, response)
	}

}

func PowerOffRequest(config *ProxmoxConfig, vm VM) (*VMPower, error) {
	// ----- SECURITY CHECK -----
	// make sure VM is not critical

	criticalMembers, err := getPoolMembers(config, CRITICAL_POOL)
	if err != nil {
		return nil, fmt.Errorf("could not verify members of critical pool: %v", err)
	}

	// return unauthorized if error or vm is critical
	if isCritical, err := isVmCritical(vm, &criticalMembers); err != nil || isCritical {
		return nil, fmt.Errorf("not authorized to power off VMID %d: %v", vm.VMID, err)
	}

	// Create HTTP client with SSL verification based on config
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !config.VerifySSL},
	}
	client := &http.Client{Transport: tr}

	// Prepare status URL
	statusURL := fmt.Sprintf("https://%s:%s/api2/extjs/nodes/%s/qemu/%s/status/shutdown", config.Host, config.Port, vm.Node, strconv.Itoa(vm.VMID))

	// Create request
	req, err := http.NewRequest("POST", statusURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	// Add Authorization header with API token
	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s", config.APIToken))

	// Make request
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to shut down VM: %v", err)
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read VM shutdown response: %v", err)
	}

	// Parse response
	var apiResp VMPower
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse VM shutdown response: %v", err)
	}

	return &apiResp, nil
}

func StopRequest(config *ProxmoxConfig, vm VM) (*VMPower, error) {

	// Create HTTP client with SSL verification based on config
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !config.VerifySSL},
	}
	client := &http.Client{Transport: tr}

	// Prepare status URL
	statusURL := fmt.Sprintf("https://%s:%s/api2/extjs/nodes/%s/qemu/%s/status/stop", config.Host, config.Port, vm.Node, strconv.Itoa(vm.VMID))

	// Create request
	req, err := http.NewRequest("POST", statusURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	// Add Authorization header with API token
	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s", config.APIToken))

	// Make request
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to stop VM: %v", err)
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read VM stop response: %v", err)
	}

	// Parse response
	var apiResp VMPower
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse VM stop response: %v", err)
	}

	return &apiResp, nil
}

/*
 * ====== POWERING ON VIRTUAL MACHINES ======
 * POST requires "vmid" and "node" fields
 */
func PowerOnVirtualMachine(c *gin.Context) {
	session := sessions.Default(c)
	username := session.Get("username")
	isAdmin := session.Get("is_admin")

	// Make sure user is admin (redundant with middleware)
	if !isAdmin.(bool) {
		log.Printf("Unauthorized access attempt by user %s", username)
		c.JSON(http.StatusForbidden, gin.H{
			"error": "Only admin users can access vm data",
		})
		return
	}

	// store proxmox config
	var config *ProxmoxConfig
	var err error
	config, err = LoadProxmoxConfig()
	if err != nil {
		log.Printf("Configuration error for user %s: %v", username, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to load Proxmox configuration: %v", err),
		})
		return
	}

	// If no proxmox host specified, return empty repsonse
	if config.Host == "" {
		log.Printf("No proxmox server configured")
		c.JSON(http.StatusOK, VirtualMachineResponse{VirtualMachines: []VirtualResource{}})
		return
	}

	// If no nodes specified, return empty response
	if len(config.Nodes) == 0 {
		log.Printf("No nodes configured for user %s", username)
		c.JSON(http.StatusOK, ResourceUsageResponse{Nodes: []NodeResourceUsage{}})
		return
	}

	// get req.VMID, req.Node
	var req VM
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: must include 'vmid' and 'node'"})
		return
	}

	// log request on backend
	log.Printf("User %s requested to power on VM %d on node %s", username, req.VMID, req.Node)
	var response *VMPower

	response, err = PowerOnRequest(config, req)

	// If we have error , return error status
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed to power on virtual machine",
			"details": err.Error(),
		})
		return
	}

	var finalResponse VMPowerResponse
	finalResponse.Success = response.Success

	if finalResponse.Success == 1 {
		log.Printf("Successfully powered on VMID %s for %s", strconv.Itoa(req.VMID), username)
		c.JSON(http.StatusOK, response)
	} else {
		log.Printf("Failed to power on VMID %s for %s", strconv.Itoa(req.VMID), username)
		c.JSON(http.StatusOK, response)
	}

}

func PowerOnRequest(config *ProxmoxConfig, vm VM) (*VMPower, error) {

	// ----- SECURITY CHECK -----
	// make sure VM is not critical

	criticalMembers, err := getPoolMembers(config, CRITICAL_POOL)
	if err != nil {
		return nil, fmt.Errorf("could not verify members of critical pool: %v", err)
	}

	// return unauthorized if error or vm is critical
	if isCritical, err := isVmCritical(vm, &criticalMembers); err != nil || isCritical {
		return nil, fmt.Errorf("not authorized to power on VMID %d: %v", vm.VMID, err)
	}

	// Create HTTP client with SSL verification based on config
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !config.VerifySSL},
	}
	client := &http.Client{Transport: tr}

	// Prepare status URL
	startURL := fmt.Sprintf("https://%s:%s/api2/extjs/nodes/%s/qemu/%s/status/start", config.Host, config.Port, vm.Node, strconv.Itoa(vm.VMID))

	// Create request
	req, err := http.NewRequest("POST", startURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	// Add Authorization header with API token
	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s", config.APIToken))

	// Make request
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to turn on VM: %v", err)
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read VM power on response: %v", err)
	}

	// Parse response
	var apiResp VMPower
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse VM power on response: %v", err)
	}

	return &apiResp, nil
}

// called by cloning.CloneTemplateToPod to wait for router to finish starting
func WaitForRunning(config *ProxmoxConfig, vm VM) error {
	// Create a single HTTP client for all requests
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !config.VerifySSL},
	}
	client := &http.Client{Transport: tr}

	// Wait for status "running" with exponential backoff
	statusURL := fmt.Sprintf("https://%s:%s/api2/json/nodes/%s/qemu/%d/status/current",
		config.Host, config.Port, vm.Node, vm.VMID)

	backoff := time.Second
	maxBackoff := 30 * time.Second
	timeout := 3 * time.Minute
	startTime := time.Now()

	for {
		if time.Since(startTime) > timeout {
			return fmt.Errorf("vm failed to start within %v", timeout)
		}

		req, err := http.NewRequest("GET", statusURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create status check request: %v", err)
		}
		req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s", config.APIToken))

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("failed to check vm status: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			// Verify the VM is actually crunning
			var statusResponse struct {
				Data struct {
					Status string `json:"status"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&statusResponse); err != nil {
				return fmt.Errorf("failed to decode status response: %v", err)
			}
			if statusResponse.Data.Status == "running" {
				return nil
			}
		}

		time.Sleep(backoff)
		backoff = time.Duration(math.Min(float64(backoff*2), float64(maxBackoff)))
	}
}

// return when vm is "stopped" or on timeout
func WaitForStopped(config *ProxmoxConfig, vm VM) error {
	// Create a single HTTP client for all requests
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !config.VerifySSL},
	}
	client := &http.Client{Transport: tr}

	// Wait for status "stopped" with exponential backoff
	statusURL := fmt.Sprintf("https://%s:%s/api2/json/nodes/%s/qemu/%d/status/current",
		config.Host, config.Port, vm.Node, vm.VMID)

	backoff := time.Second
	maxBackoff := 30 * time.Second
	timeout := 3 * time.Minute
	startTime := time.Now()

	for {
		if time.Since(startTime) > timeout {
			return fmt.Errorf("vm failed to stop within %v", timeout)
		}

		req, err := http.NewRequest("GET", statusURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create status check request: %v", err)
		}
		req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s", config.APIToken))

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("failed to check vm status: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			// Verify the VM is actually stopped
			var statusResponse struct {
				Data struct {
					Status string `json:"status"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&statusResponse); err != nil {
				return fmt.Errorf("failed to decode status response: %v", err)
			}
			if statusResponse.Data.Status == "stopped" {
				return nil
			}
		}

		time.Sleep(backoff)
		backoff = time.Duration(math.Min(float64(backoff*2), float64(maxBackoff)))
	}
}

// return whether or not a vm is in a resource pool member list
func isVmCritical(vm VM, poolMembers *[]VirtualResource) (isInCritical bool, err error) {
	for _, poolVm := range *poolMembers {
		if poolVm.VmId == vm.VMID {
			return true, nil
		}
	}

	return false, nil
}

func getPoolMembers(config *ProxmoxConfig, pool string) (members []VirtualResource, err error) {
	// Create a single HTTP client for all requests
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !config.VerifySSL},
	}
	client := &http.Client{Transport: tr}

	// Wait for status "stopped" with exponential backoff
	poolURL := fmt.Sprintf("https://%s:%s/api2/json/pools",
		config.Host, config.Port)

	req, err := http.NewRequest("GET", poolURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create pool request: %v", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s", config.APIToken))

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get critical pool: %v", err)
	}
	defer resp.Body.Close()

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read pool response body: %v", err)
	}

	// Parse
	var poolResponse PoolResponse
	err = json.Unmarshal(respBody, &poolResponse)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal pool response body: %v", err)
	}

	for _, responsePool := range poolResponse.Data {
		if responsePool.Poolid == pool {
			return responsePool.Members, nil
		}
	}

	return nil, fmt.Errorf("failed to identify resource pool with id %s", CRITICAL_POOL)

}
