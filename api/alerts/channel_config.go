package alerts

import (
	"fmt"
	"sort"
	"strings"
)

func channelFields(kind string) (safe, credentials []string) {
	switch kind {
	case "discord", "slack", "webhook":
		return nil, []string{"url"}
	case "telegram":
		return []string{"chat_id"}, []string{"bot_token", "api_url"}
	case "email":
		return []string{"smtp_host", "smtp_port", "from", "to"}, []string{"username", "password"}
	default:
		return nil, nil
	}
}

// PublicChannelConfig only returns known non-secret fields; configured credential
// names tell the UI what can be retained without ever returning their values.
func PublicChannelConfig(kind string, config map[string]interface{}) (map[string]interface{}, []string) {
	safe, credentials := channelFields(kind)
	public := make(map[string]interface{}, len(safe))
	configured := make([]string, 0, len(credentials))
	for _, key := range safe {
		if value, ok := config[key]; ok {
			if key == "smtp_port" {
				if number, valid := value.(float64); valid {
					public[key] = number
				}
				if number, valid := value.(int); valid {
					public[key] = number
				}
			} else if text, valid := value.(string); valid {
				public[key] = text
			}
		}
	}
	for _, key := range credentials {
		if requiredString(config, key) != "" {
			configured = append(configured, key)
		}
	}
	sort.Strings(configured)
	return public, configured
}

// MergeChannelConfig applies a replacement patch to the locked stored config.
// Omission retains existing credentials only for the same channel type.
func MergeChannelConfig(previousType, kind string, previous, patch map[string]interface{}) (map[string]interface{}, error) {
	safe, credentials := channelFields(kind)
	if safe == nil && credentials == nil {
		return nil, fmt.Errorf("unsupported notification channel type")
	}
	result := make(map[string]interface{}, len(previous)+len(patch))
	if previousType == kind {
		for key, value := range previous {
			result[key] = value
		}
	}
	allowed := make(map[string]bool, len(safe)+len(credentials))
	secret := make(map[string]bool, len(credentials))
	for _, key := range safe {
		allowed[key] = true
	}
	for _, key := range credentials {
		allowed[key] = true
		secret[key] = true
	}
	for key, value := range patch {
		if !allowed[key] {
			return nil, fmt.Errorf("unsupported notification configuration field")
		}
		if secret[key] {
			if value == nil {
				delete(result, key)
				continue
			}
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return nil, fmt.Errorf("%s must be a nonempty string or null", key)
			}
			result[key] = text
		} else if value == nil {
			delete(result, key)
		} else {
			result[key] = value
		}
	}
	if err := ValidateChannelConfig(kind, result); err != nil {
		return nil, err
	}
	return result, nil
}
