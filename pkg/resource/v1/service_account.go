package v1

type ServiceAccount struct {
	Token      string               `json:"token,omitempty"`
	Roles      []ServiceAccountRole `json:"roles,omitempty"`
	WorkerName string               `json:"workerName,omitempty"`

	Meta
}

func (serviceAccount *ServiceAccount) SetVersion(_ uint64) {}

func (serviceAccount *ServiceAccount) Match(filter Filter) bool {
	return false
}
