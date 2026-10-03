package identity

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This is an explicit IAM policy subset: action/resource matching, StringEquals
// conditions and explicit deny. Unsupported syntax fails at startup.
type Strings []string

func (s *Strings) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		return fmt.Errorf("policy values must be strings or string arrays")
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		*s = []string{one}
		return nil
	}
	return json.Unmarshal(raw, (*[]string)(s))
}

type Policy struct {
	Version   string      `json:"Version"`
	Statement []Statement `json:"Statement"`
}
type Statement struct {
	Sid       string                        `json:"Sid,omitempty"`
	Effect    string                        `json:"Effect"`
	Action    Strings                       `json:"Action"`
	Resource  Strings                       `json:"Resource"`
	Condition map[string]map[string]Strings `json:"Condition,omitempty"`
}

func (p Policy) Validate() error {
	if p.Version != "2012-10-17" || len(p.Statement) == 0 {
		return fmt.Errorf("policy requires Version 2012-10-17 and Statement array")
	}
	for _, s := range p.Statement {
		if s.Effect != "Allow" && s.Effect != "Deny" || len(s.Action) == 0 || len(s.Resource) == 0 {
			return fmt.Errorf("policy statement requires Effect, Action and Resource")
		}
		for _, a := range s.Action {
			if a != "*" && !strings.HasPrefix(a, "s3:") || strings.Contains(a, "${") || len(a) > 256 {
				return fmt.Errorf("only S3 policy actions are supported")
			}
		}
		for _, r := range s.Resource {
			if r != "*" && !strings.HasPrefix(r, "arn:aws:s3:::") || strings.Contains(r, "${") || len(r) > 2048 {
				return fmt.Errorf("only literal S3 resource patterns are supported")
			}
		}
		for op, conditions := range s.Condition {
			if op != "StringEquals" || len(conditions) == 0 {
				return fmt.Errorf("only nonempty StringEquals conditions are supported")
			}
			for key, values := range conditions {
				if !tagCondition(key) || len(values) == 0 {
					return fmt.Errorf("conditions require aws:ResourceTag/ or aws:PrincipalTag/ keys")
				}
				for _, v := range values {
					if strings.Contains(v, "${") && (!strings.HasPrefix(v, "${aws:PrincipalTag/") || !strings.HasSuffix(v, "}") || strings.Count(v, "${") != 1) {
						return fmt.Errorf("only whole-value principal tag substitutions are supported")
					}
				}
			}
		}
	}
	return nil
}

func tagCondition(s string) bool {
	for _, prefix := range []string{"aws:ResourceTag/", "aws:PrincipalTag/"} {
		if strings.HasPrefix(s, prefix) && len(s) > len(prefix) {
			return true
		}
	}
	return false
}

// wildcard implements IAM's * and ? matching, including slashes in object keys.
func wildcard(pattern, value string) bool {
	i, j, star, retry := 0, 0, -1, 0
	for j < len(value) {
		if i < len(pattern) && (pattern[i] == '?' || pattern[i] == value[j]) {
			i++
			j++
			continue
		}
		if i < len(pattern) && pattern[i] == '*' {
			star = i
			i++
			retry = j
			continue
		}
		if star < 0 {
			return false
		}
		i = star + 1
		retry++
		j = retry
	}
	for i < len(pattern) && pattern[i] == '*' {
		i++
	}
	return i == len(pattern)
}
func matches(patterns []string, value string) bool {
	for _, p := range patterns {
		if wildcard(p, value) {
			return true
		}
	}
	return false
}
func (p Policy) Allows(action, resource string, principalTags, resourceTags map[string]string) bool {
	allowed := false
	for _, s := range p.Statement {
		if !matches(s.Action, action) || !matches(s.Resource, resource) {
			continue
		}
		match := true
		for _, conditions := range s.Condition {
			for key, values := range conditions {
				var actual string
				var exists bool
				if strings.HasPrefix(key, "aws:ResourceTag/") {
					actual, exists = resourceTags[strings.TrimPrefix(key, "aws:ResourceTag/")]
				} else {
					actual, exists = principalTags[strings.TrimPrefix(key, "aws:PrincipalTag/")]
				}
				found := false
				for _, expected := range values {
					if strings.HasPrefix(expected, "${aws:PrincipalTag/") {
						var ok bool
						expected, ok = principalTags[strings.TrimSuffix(strings.TrimPrefix(expected, "${aws:PrincipalTag/"), "}")]
						if !ok {
							continue
						}
					}
					if exists && actual == expected {
						found = true
					}
				}
				if !found {
					match = false
				}
			}
		}
		if match {
			if s.Effect == "Deny" {
				return false
			}
			allowed = true
		}
	}
	return allowed
}
