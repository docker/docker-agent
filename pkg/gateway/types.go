package gateway

type topLevel struct {
	Catalog Catalog `json:"registry"`
}

type Catalog map[string]Server

type Server struct {
	Type    string   `json:"type"`
	Secrets []Secret `json:"secrets,omitempty"`
	Remote  Remote   `json:"remote"`
}

type Remote struct {
	URL           string `json:"url"`
	TransportType string `json:"transport_type"`
}

type Secret struct {
	Name    string `json:"name"`
	Env     string `json:"env"`
	Example string `json:"example"`
}
