package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ai-club/sandbox-host/internal/egress"
)

func parsePolicy(raw json.RawMessage, ceiling egress.Policy) (egress.Policy, any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ceiling, policyResponse(ceiling), nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch mode {
		case "deny-all":
			return egress.Policy{}, map[string]any{"mode": "deny-all"}, nil
		case "allow-all":
			return egress.Policy{}, nil, errors.New("allow-all exceeds the administrator ceiling")
		default:
			return egress.Policy{}, nil, errors.New("unknown network policy mode")
		}
	}
	var object struct {
		Mode  string          `json:"mode"`
		Allow json.RawMessage `json:"allow"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return egress.Policy{}, nil, fmt.Errorf("invalid network policy: %w", err)
	}
	if object.Mode == "deny-all" {
		return egress.Policy{}, map[string]any{"mode": "deny-all"}, nil
	}
	if object.Mode == "allow-all" {
		return egress.Policy{}, nil, errors.New("allow-all exceeds the administrator ceiling")
	}
	var domains []string
	if len(object.Allow) > 0 {
		if err := json.Unmarshal(object.Allow, &domains); err != nil {
			return egress.Policy{}, nil, errors.New("record-form transforms/forwarding are not enabled on this deployment")
		}
	} else {
		var legacy struct {
			AllowedDomains []string `json:"allowedDomains"`
		}
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return egress.Policy{}, nil, err
		}
		domains = legacy.AllowedDomains
	}
	requested := egress.Policy{Rules: make([]egress.Rule, 0, len(domains))}
	for _, domain := range domains {
		requested.Rules = append(requested.Rules, egress.Rule{Domain: domain})
	}
	if err := requested.Validate(); err != nil {
		return egress.Policy{}, nil, err
	}
	effective := egress.Intersect(requested, ceiling)
	if len(effective.Rules) != len(requested.Rules) {
		return egress.Policy{}, nil, errors.New("one or more requested domains exceed the administrator ceiling")
	}
	return effective, policyResponse(effective), nil
}

func policyResponse(policy egress.Policy) any {
	domains := make([]string, 0, len(policy.Rules))
	for _, rule := range policy.Rules {
		domains = append(domains, rule.Domain)
	}
	return map[string]any{"mode": "custom", "allowedDomains": domains}
}
