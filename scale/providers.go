package scale

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Provider rents a machine that can run the mojojojo worker.
//
// Two exist: RunPod for GPUs (per-second billing, minutes to start) and
// Hetzner for CPU (hourly billing, ~40s to start). Both are wrapped behind the
// same three calls so the manager never has to care which one it is talking to.
type Provider interface {
	Name() string
	Offers() []Offer
	// Launch returns a worker id. The worker is not ready to serve when this
	// returns; the manager polls Status.
	Launch(offer Offer, cloudInit string) (string, error)
	Status(id string) (Worker, error)
	Terminate(id string) error
}

// Worker is a rented machine.
type Worker struct {
	ID       string
	Provider string
	Kind     Kind
	Type     string
	USDHour  float64
	IP       string
	Ready    bool
	Started  time.Time
}

// DryRun wraps a provider and refuses to spend money.
//
// This is the default, and it is not a testing convenience: it means a
// misconfiguration, a runaway queue, or a bug in the policy costs nothing at
// all until somebody deliberately turns spending on.
type DryRun struct{ Inner Provider }

func (d DryRun) Name() string    { return d.Inner.Name() + " (dry-run)" }
func (d DryRun) Offers() []Offer { return d.Inner.Offers() }

func (d DryRun) Launch(offer Offer, _ string) (string, error) {
	return fmt.Sprintf("dryrun-%s-%s-%d", d.Inner.Name(), offer.Type,
		time.Now().UnixNano()), nil
}

func (d DryRun) Status(id string) (Worker, error) {
	return Worker{ID: id, Provider: d.Name(), Ready: false}, nil
}

func (d DryRun) Terminate(string) error { return nil }

// --- RunPod: GPUs, per-second billing ---

type RunPod struct {
	APIKey string
	Image  string
	client *http.Client
}

func NewRunPod(apiKey, image string) *RunPod {
	if image == "" {
		image = "runpod/pytorch:2.4.0-py3.11-cuda12.4.1-devel-ubuntu22.04"
	}
	return &RunPod{APIKey: apiKey, Image: image,
		client: &http.Client{Timeout: 60 * time.Second}}
}

func (r *RunPod) Name() string { return "runpod" }

// Offers are the GPUs worth renting for this workload. The controller uses
// conservative public on-demand rates for its margin gate; a cheaper community
// allocation only increases the realised margin.
func (r *RunPod) Offers() []Offer {
	return []Offer{
		{Provider: "runpod", Type: "NVIDIA RTX A5000", USDHour: 0.27, Kind: GPU},
		{Provider: "runpod", Type: "NVIDIA GeForce RTX 4090", USDHour: 0.69, Kind: GPU},
		{Provider: "runpod", Type: "NVIDIA GeForce RTX 5090", USDHour: 0.99, Kind: GPU},
	}
}

func (r *RunPod) Launch(offer Offer, cloudInit string) (string, error) {
	if r.APIKey == "" {
		return "", errors.New("runpod: no API key")
	}
	body := map[string]any{
		"cloudType":         "COMMUNITY",
		"gpuTypeIds":        []string{offer.Type},
		"imageName":         r.Image,
		"name":              "mojojojo-worker",
		"containerDiskInGb": 30,
		"ports":             []string{"4342/http"},
		"env":               map[string]string{"MOJOJOJO_BOOTSTRAP": cloudInit},
		"dockerStartCmd":    []string{"bash", "-lc", cloudInit},
	}
	var out struct {
		ID     string `json:"id"`
		Status string `json:"desiredStatus"`
	}
	if err := r.call("POST", "https://rest.runpod.io/v1/pods", body, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("runpod: no pod id returned")
	}
	return out.ID, nil
}

func (r *RunPod) Status(id string) (Worker, error) {
	var out struct {
		ID            string `json:"id"`
		DesiredStatus string `json:"desiredStatus"`
		PublicIP      string `json:"publicIp"`
		Runtime       *struct {
			Ports []struct {
				IP          string `json:"ip"`
				PublicPort  int    `json:"publicPort"`
				PrivatePort int    `json:"privatePort"`
			} `json:"ports"`
		} `json:"runtime"`
	}
	if err := r.call("GET", "https://rest.runpod.io/v1/pods/"+id, nil, &out); err != nil {
		return Worker{}, err
	}
	w := Worker{ID: id, Provider: "runpod", Kind: GPU, IP: out.PublicIP}
	if out.Runtime != nil {
		for _, p := range out.Runtime.Ports {
			if p.PrivatePort == 4342 && p.IP != "" {
				w.IP = fmt.Sprintf("%s:%d", p.IP, p.PublicPort)
				w.Ready = strings.EqualFold(out.DesiredStatus, "RUNNING")
			}
		}
	}
	return w, nil
}

func (r *RunPod) Terminate(id string) error {
	return r.call("DELETE", "https://rest.runpod.io/v1/pods/"+id, nil, nil)
}

func (r *RunPod) call(method, url string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("runpod %s %s: %s: %s", method, url, resp.Status,
			strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// --- Hetzner: CPU, hourly billing ---

type Hetzner struct {
	Token    string
	Image    string
	Location string
	SSHKeys  []string
	client   *http.Client
}

func NewHetzner(token string) *Hetzner {
	return &Hetzner{
		Token: token, Image: "ubuntu-24.04", Location: "hel1",
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

func (h *Hetzner) Name() string { return "hetzner" }

func (h *Hetzner) Offers() []Offer {
	return []Offer{
		{Provider: "hetzner", Type: "cx43", USDHour: 0.0296, Kind: CPU},
		{Provider: "hetzner", Type: "cx53", USDHour: 0.0561, Kind: CPU},
		{Provider: "hetzner", Type: "ccx13", USDHour: 0.0809, Kind: CPU},
	}
}

func (h *Hetzner) Launch(offer Offer, cloudInit string) (string, error) {
	if h.Token == "" {
		return "", errors.New("hetzner: no API token")
	}
	body := map[string]any{
		"name":        fmt.Sprintf("mojojojo-%d", time.Now().Unix()),
		"server_type": offer.Type,
		"image":       h.Image,
		"location":    h.Location,
		"user_data":   cloudInit,
		"labels":      map[string]string{"app": "mojojojo", "role": "worker"},
	}
	if len(h.SSHKeys) > 0 {
		body["ssh_keys"] = h.SSHKeys
	}
	var out struct {
		Server struct {
			ID     int    `json:"id"`
			Status string `json:"status"`
		} `json:"server"`
	}
	if err := h.call("POST", "https://api.hetzner.cloud/v1/servers", body, &out); err != nil {
		return "", err
	}
	if out.Server.ID == 0 {
		return "", errors.New("hetzner: no server id returned")
	}
	return fmt.Sprint(out.Server.ID), nil
}

func (h *Hetzner) Status(id string) (Worker, error) {
	var out struct {
		Server struct {
			Status     string                             `json:"status"`
			PublicNet  struct{ IPv4 struct{ IP string } } `json:"public_net"`
			ServerType struct{ Name string }              `json:"server_type"`
		} `json:"server"`
	}
	if err := h.call("GET", "https://api.hetzner.cloud/v1/servers/"+id, nil, &out); err != nil {
		return Worker{}, err
	}
	return Worker{
		ID: id, Provider: "hetzner", Kind: CPU,
		Type:  out.Server.ServerType.Name,
		IP:    out.Server.PublicNet.IPv4.IP,
		Ready: out.Server.Status == "running" && out.Server.PublicNet.IPv4.IP != "",
	}, nil
}

func (h *Hetzner) Terminate(id string) error {
	return h.call("DELETE", "https://api.hetzner.cloud/v1/servers/"+id, nil, nil)
}

func (h *Hetzner) call(method, url string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("hetzner %s: %s: %s", method, resp.Status,
			strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// FromEnv builds the providers this deployment is configured for.
//
// Absent credentials mean absent providers, not errors: running without any is
// the normal state, and it simply means the queue waits for the local box.
func FromEnv() []Provider {
	var out []Provider
	dry := os.Getenv("SCALE_DRY_RUN") != "false" // opt *in* to spending
	if key := os.Getenv("RUNPOD_API_KEY"); key != "" {
		var p Provider = NewRunPod(key, os.Getenv("RUNPOD_IMAGE"))
		if dry {
			p = DryRun{Inner: p}
		}
		out = append(out, p)
	}
	if token := os.Getenv("HETZNER_API_TOKEN"); token != "" {
		var p Provider = NewHetzner(token)
		if dry {
			p = DryRun{Inner: p}
		}
		out = append(out, p)
	}
	return out
}
