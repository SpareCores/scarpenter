/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/client"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/service"
)

// TokenEnv is the environment variable of the API token (a bearer token; the
// chart's credentials Secret provides it).
const TokenEnv = "UPCLOUD_TOKEN"

// ErrNoToken is returned by NewFromEnv when TokenEnv is empty.
var ErrNoToken = errors.New("UPCLOUD_TOKEN is not set")

// Client is the real API: the SDK service for everything it covers, and the
// ruleset calls written over the SDK's HTTP client. One client serves every
// zone (the API is global; the zone is a parameter of the calls).
type Client struct {
	*service.Service
	http *client.Client
}

// New builds a client with a bearer token. It makes no call. The SDK retries
// 5xx answers itself (3 tries); per-call deadlines are the caller's.
func New(token, userAgent string, opts ...client.ConfigFn) *Client {
	opts = append([]client.ConfigFn{client.WithBearerAuth(token), client.WithTimeout(2 * time.Minute)}, opts...)
	c := client.New("", "", opts...)
	if userAgent != "" {
		c.UserAgent = userAgent
	}
	return &Client{Service: service.New(c), http: c}
}

// NewFromEnv builds a client from UPCLOUD_TOKEN.
func NewFromEnv(userAgent string, opts ...client.ConfigFn) (*Client, error) {
	token := os.Getenv(TokenEnv)
	if token == "" {
		return nil, ErrNoToken
	}
	return New(token, userAgent, opts...), nil
}

type rulesetJSON struct {
	UUID                   string          `json:"uuid"`
	Name                   string          `json:"name"`
	Description            string          `json:"description"`
	Stateful               bool            `json:"stateful"`
	Enabled                bool            `json:"enabled"`
	Version                int             `json:"version"`
	DefaultDNSRulesEnabled *bool           `json:"default_dns_rules_enabled,omitempty"`
	Labels                 []upcloud.Label `json:"labels,omitempty"`
}

func (r rulesetJSON) ruleset() Ruleset {
	return Ruleset{
		UUID: r.UUID, Name: r.Name, Description: r.Description, Stateful: r.Stateful, Enabled: r.Enabled,
		Version: r.Version, Labels: r.Labels,
	}
}

type ruleJSON struct {
	Action    string `json:"action"`
	Direction string `json:"direction"`
	Family    string `json:"family,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	// SourceCIDR and DestinationCIDR are the v9 spellings of the address ranges.
	SourceCIDR      string `json:"source_address_cidr,omitempty"`
	DestinationCIDR string `json:"destination_address_cidr,omitempty"`
	PortStart       int    `json:"destination_port_start,omitempty"`
	PortEnd         int    `json:"destination_port_end,omitempty"`
	Comment         string `json:"comment,omitempty"`
	Enabled         bool   `json:"enabled"`
}

func ruleBody(r RulesetRule) ruleJSON {
	j := ruleJSON{
		Action: r.Action, Direction: r.Direction, Family: r.Family, Protocol: r.Protocol, Comment: r.Comment,
		PortStart: r.PortStart, PortEnd: r.PortEnd, Enabled: true,
	}
	if r.Direction == "out" {
		j.DestinationCIDR = r.CIDR
	} else {
		j.SourceCIDR = r.CIDR
	}
	return j
}

const rulesetPath = "/firewall-ruleset"

// GetFirewallRulesets lists the account's rulesets.
func (c *Client) GetFirewallRulesets(ctx context.Context) ([]Ruleset, error) {
	body, err := c.http.Get(ctx, rulesetPath)
	if err != nil {
		return nil, ParseError(err)
	}
	var list []rulesetJSON
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decoding the ruleset list: %w", err)
	}
	out := make([]Ruleset, len(list))
	for i, r := range list {
		out[i] = r.ruleset()
	}
	return out, nil
}

// GetFirewallRuleset returns one ruleset.
func (c *Client) GetFirewallRuleset(ctx context.Context, uuid string) (*Ruleset, error) {
	body, err := c.http.Get(ctx, rulesetPath+"/"+url.PathEscape(uuid))
	if err != nil {
		return nil, ParseError(err)
	}
	var j rulesetJSON
	if err := json.Unmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("decoding the ruleset: %w", err)
	}
	r := j.ruleset()
	return &r, nil
}

// CreateFirewallRuleset creates the ruleset, then its rules in one call.
func (c *Client) CreateFirewallRuleset(ctx context.Context, r CreateRulesetRequest) (*Ruleset, error) {
	payload, err := json.Marshal(rulesetJSON{
		Name: r.Name, Description: r.Description, Stateful: r.Stateful, Enabled: true, Labels: r.Labels,
	})
	if err != nil {
		return nil, err
	}
	body, err := c.http.Post(ctx, rulesetPath, payload)
	if err != nil {
		return nil, ParseError(err)
	}
	var j rulesetJSON
	if err := json.Unmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("decoding the created ruleset: %w", err)
	}
	if len(r.Rules) > 0 {
		rules := make([]ruleJSON, len(r.Rules))
		for i, rule := range r.Rules {
			rules[i] = ruleBody(rule)
		}
		payload, err = json.Marshal(map[string]any{"rules": rules})
		if err != nil {
			return nil, err
		}
		if _, err = c.http.Post(ctx, rulesetPath+"/"+url.PathEscape(j.UUID)+"/rule", payload); err != nil {
			return nil, ParseError(err)
		}
	}
	out := j.ruleset()
	return &out, nil
}

// DeleteFirewallRuleset deletes a ruleset that is not attached.
func (c *Client) DeleteFirewallRuleset(ctx context.Context, uuid string) error {
	_, err := c.http.Delete(ctx, rulesetPath+"/"+url.PathEscape(uuid))
	return ParseError(err)
}

type rulesetRef struct {
	FirewallRuleset struct {
		UUID string `json:"firewall_ruleset_uuid"`
	} `json:"firewall_ruleset"`
}

func ref(uuid string) []byte {
	var r rulesetRef
	r.FirewallRuleset.UUID = uuid
	b, _ := json.Marshal(r)
	return b
}

// AttachPrivateFirewallRuleset attaches a ruleset to the server's private
// interfaces.
func (c *Client) AttachPrivateFirewallRuleset(ctx context.Context, serverUUID, rulesetUUID string) error {
	_, err := c.http.Post(ctx, "/server/"+url.PathEscape(serverUUID)+"/attach_firewall_ruleset/private", ref(rulesetUUID))
	return ParseError(err)
}

// DetachPrivateFirewallRuleset detaches it again.
func (c *Client) DetachPrivateFirewallRuleset(ctx context.Context, serverUUID, rulesetUUID string) error {
	_, err := c.http.Post(ctx, "/server/"+url.PathEscape(serverUUID)+"/detach_firewall_ruleset/private", ref(rulesetUUID))
	return ParseError(err)
}

// ParseError turns the SDK client's HTTP error into the *upcloud.Problem the
// service methods return (the service package's own helper is unexported), so
// that the ruleset calls fail the same way the others do. Other errors pass
// through.
func ParseError(err error) error {
	if err == nil {
		return nil
	}
	ce, ok := errors.AsType[*client.Error](err)
	if !ok {
		return err
	}
	prob := &upcloud.Problem{}
	if ce.Type == client.ErrorTypeProblem {
		if json.Unmarshal(ce.ResponseBody, prob) != nil {
			return err
		}
		if prob.Status == 0 {
			prob.Status = ce.ErrorCode
		}
		return prob
	}
	var legacy struct {
		Error struct {
			Code    string `json:"error_code"`
			Message string `json:"error_message"`
		} `json:"error"`
	}
	if json.Unmarshal(ce.ResponseBody, &legacy) != nil {
		return err
	}
	prob.Type, prob.Title, prob.Status = legacy.Error.Code, legacy.Error.Message, ce.ErrorCode
	return prob
}
