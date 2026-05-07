// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/BurntSushi/toml"
)

// secretFieldNames lists case-insensitive substrings that mark a field as
// a secret. Matches are deliberately broad — better to redact a non-secret
// than leak one. URL fields are included because connection URLs commonly
// carry credentials in the userinfo component.
var secretFieldNames = []string{
	"password", "pass",
	"secret",
	"key",
	"token",
	"dsn",
	"url",
}

// PrintRedacted writes the config to w in TOML form with secrets replaced
// by `***`. The transformation operates on a deep copy so the live Config
// is never mutated.
func PrintRedacted(c Config) (string, error) {
	cp := redactCopy(reflect.ValueOf(c)).Interface().(Config)
	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(cp); err != nil {
		return "", fmt.Errorf("config: encode: %w", err)
	}
	return buf.String(), nil
}

func redactCopy(v reflect.Value) reflect.Value {
	out := reflect.New(v.Type()).Elem()
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			fv := v.Field(i)
			ft := v.Type().Field(i)
			if !ft.IsExported() {
				continue
			}
			if fv.Kind() == reflect.Struct {
				out.Field(i).Set(redactCopy(fv))
				continue
			}
			if shouldRedact(ft.Name) && fv.Kind() == reflect.String && fv.String() != "" {
				out.Field(i).SetString("***")
				continue
			}
			out.Field(i).Set(fv)
		}
	default:
		out.Set(v)
	}
	return out
}

func shouldRedact(fieldName string) bool {
	lower := strings.ToLower(fieldName)
	for _, needle := range secretFieldNames {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}
