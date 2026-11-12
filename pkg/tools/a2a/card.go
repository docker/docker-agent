package a2a

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
)

// oauthFlowNames lists OAuth flows in A2A schema order.
var oauthFlowNames = []a2a.OAuthFlowName{
	a2a.AuthorizationCodeOAuthFlowName,
	a2a.ClientCredentialsOAuthFlowName,
	a2a.ImplicitOAuthFlowName,
	a2a.PasswordOAuthFlowName,
	a2a.DeviceCodeOAuthFlowName,
}

// parseAgentCard reads A2A 1.0 and 0.3 cards. The v0 parser handles both shapes.
func parseAgentCard(body []byte) (*a2a.AgentCard, error) {
	body, err := normalizeOAuthFlows(body)
	if err != nil {
		return nil, err
	}
	card, err := a2av0.NewAgentCardParser()(body)
	if err != nil {
		return nil, err
	}
	legacy, err := legacyInterfaces(body)
	if err != nil {
		return nil, err
	}
	if legacy != nil {
		card.SupportedInterfaces = legacy
	}
	if err := validateInterfaces(card.SupportedInterfaces); err != nil {
		return nil, err
	}
	return card, nil
}

// normalizeOAuthFlows keeps one flow per 0.3 oauth2 scheme and drops schemes
// with none: 0.3 allows any number, the SDK models exactly one, and the toolset
// never reads them.
func normalizeOAuthFlows(body []byte) ([]byte, error) {
	var card map[string]json.RawMessage
	if err := json.Unmarshal(body, &card); err != nil {
		return nil, err
	}
	var schemes map[string]map[string]json.RawMessage
	if json.Unmarshal(card["securitySchemes"], &schemes) != nil {
		// Absent or malformed: the SDK parser decides.
		return body, nil
	}

	changed := false
	for name, scheme := range schemes {
		var schemeType string
		if err := json.Unmarshal(scheme["type"], &schemeType); err != nil || schemeType != "oauth2" {
			continue
		}
		var flows map[a2a.OAuthFlowName]json.RawMessage
		_ = json.Unmarshal(scheme["flows"], &flows)
		i := slices.IndexFunc(oauthFlowNames, func(n a2a.OAuthFlowName) bool { _, ok := flows[n]; return ok })
		if i >= 0 && len(flows) == 1 {
			continue
		}

		changed = true
		if i < 0 {
			delete(schemes, name)
			continue
		}
		kept, err := json.Marshal(map[a2a.OAuthFlowName]json.RawMessage{oauthFlowNames[i]: flows[oauthFlowNames[i]]})
		if err != nil {
			return nil, err
		}
		scheme["flows"] = kept
	}
	if !changed {
		return body, nil
	}

	normalized, err := json.Marshal(schemes)
	if err != nil {
		return nil, err
	}
	card["securitySchemes"] = normalized
	return json.Marshal(card)
}

// legacyInterfaces lists a 0.3 card's url and additionalInterfaces. The v0
// parser drops every additional entry on the primary URL, but 0.3 lets other
// transports share it.
func legacyInterfaces(body []byte) ([]*a2a.AgentInterface, error) {
	var legacy struct {
		SupportedInterfaces  []json.RawMessage     `json:"supportedInterfaces"`
		URL                  string                `json:"url"`
		PreferredTransport   a2a.TransportProtocol `json:"preferredTransport"`
		ProtocolVersion      a2a.ProtocolVersion   `json:"protocolVersion"`
		AdditionalInterfaces []struct {
			URL       string                `json:"url"`
			Transport a2a.TransportProtocol `json:"transport"`
		} `json:"additionalInterfaces"`
	}
	if err := json.Unmarshal(body, &legacy); err != nil {
		return nil, err
	}
	if len(legacy.SupportedInterfaces) > 0 {
		return nil, nil
	}

	version := cmp.Or(legacy.ProtocolVersion, a2av0.Version)
	var interfaces []*a2a.AgentInterface
	add := func(url string, transport a2a.TransportProtocol) {
		iface := &a2a.AgentInterface{URL: url, ProtocolBinding: transport, ProtocolVersion: version}
		if !slices.ContainsFunc(interfaces, func(other *a2a.AgentInterface) bool { return *other == *iface }) {
			interfaces = append(interfaces, iface)
		}
	}
	if legacy.URL != "" && legacy.PreferredTransport != "" {
		add(legacy.URL, legacy.PreferredTransport)
	}
	for _, iface := range legacy.AdditionalInterfaces {
		// As before the 1.0 migration: an entry missing its url or transport is skipped.
		if iface.URL != "" && iface.Transport != "" {
			add(iface.URL, iface.Transport)
		}
	}
	return interfaces, nil
}

// validateInterfaces rejects entries the SDK client factory would dereference or cannot dial.
func validateInterfaces(interfaces []*a2a.AgentInterface) error {
	for i, iface := range interfaces {
		switch {
		case iface == nil:
			return fmt.Errorf("supportedInterfaces[%d] is null", i)
		case iface.URL == "":
			return fmt.Errorf("supportedInterfaces[%d] has no url", i)
		case iface.ProtocolBinding == "":
			return fmt.Errorf("supportedInterfaces[%d] has no protocolBinding", i)
		}
	}
	return nil
}
