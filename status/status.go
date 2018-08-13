package status

type DeployStatus struct {
	Current        Current  `json:"current,omitempty"`
	Previous       Previous `json:"previous,omitempty"`
	Done           bool     `json:"done"`
	Service        string   `json:"service,omitempty"`
	Cluster        string   `json:"cluster,omitempty"`
	TaskDefinition string   `json:"task_definition,omitempty"`
}

type Current struct {
	Desired int `json:"desired"`
	Running int `json:"running"`
	Pending int `json:"pending"`
}

type Previous struct {
	Running int `json:"running"`
	Total   int `json:"total"`
}
