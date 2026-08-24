package models

import (
	"encoding/json"
	"fmt"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

// AuthType identifies how the plugin authenticates to a SQL Server instance.
type AuthType string

const (
	AuthTypeSQL      AuthType = "sql"
	AuthTypeNTLM     AuthType = "ntlm"
	AuthTypeKerberos AuthType = "kerberos"
)

// ConnectionSettings describes how to reach one SQL Server database (either
// the SCOM Operational database or the SCOM Data Warehouse).
type ConnectionSettings struct {
	Host                   string   `json:"host"`
	Port                   string   `json:"port"`
	InstanceName           string   `json:"instanceName"` // named instance, e.g. "SCOM"; leave blank for the default instance
	Database               string   `json:"database"`
	AuthType               AuthType `json:"authType"`
	Username               string   `json:"username"`
	Domain                 string   `json:"domain"`  // used when AuthType == ntlm
	Realm                  string   `json:"realm"`   // used when AuthType == kerberos
	Encrypt                string   `json:"encrypt"` // "true" | "false" | "disable" | "strict"
	TrustServerCertificate bool     `json:"trustServerCertificate"`
}

// PluginSettings is the non-secret configuration for the data source.
type PluginSettings struct {
	Operational                ConnectionSettings    `json:"operational"`
	Warehouse                  ConnectionSettings    `json:"warehouse"`
	WarehouseSameAsOperational bool                  `json:"warehouseSameAsOperational"`
	Secrets                    *SecretPluginSettings `json:"-"`
}

// SecretPluginSettings holds values that are only ever decrypted on the backend.
type SecretPluginSettings struct {
	OperationalPassword string `json:"operationalPassword"`
	WarehousePassword   string `json:"warehousePassword"`
}

func LoadPluginSettings(source backend.DataSourceInstanceSettings) (*PluginSettings, error) {
	settings := PluginSettings{}
	if len(source.JSONData) > 0 {
		if err := json.Unmarshal(source.JSONData, &settings); err != nil {
			return nil, fmt.Errorf("could not unmarshal PluginSettings json: %w", err)
		}
	}

	settings.Secrets = loadSecretPluginSettings(source.DecryptedSecureJSONData)

	if settings.WarehouseSameAsOperational {
		settings.Warehouse = settings.Operational
		settings.Secrets.WarehousePassword = settings.Secrets.OperationalPassword
	}

	return &settings, nil
}

func loadSecretPluginSettings(source map[string]string) *SecretPluginSettings {
	return &SecretPluginSettings{
		OperationalPassword: source["operationalPassword"],
		WarehousePassword:   source["warehousePassword"],
	}
}
