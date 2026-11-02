package models

import (
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

func TestLoadPluginSettingsWarehouseSameAsOperational(t *testing.T) {
	load := func(jsonData string) *PluginSettings {
		t.Helper()
		settings, err := LoadPluginSettings(backend.DataSourceInstanceSettings{
			JSONData:                []byte(jsonData),
			DecryptedSecureJSONData: map[string]string{"operationalPassword": "secret"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return settings
	}

	// The switch shares the server and credentials; copying the database name
	// too used to point every warehouse query at OperationsManager.
	t.Run("keeps the warehouse's own database name", func(t *testing.T) {
		s := load(`{
			"warehouseSameAsOperational": true,
			"operational": {"host": "sql01", "port": "1433", "database": "OperationsManager", "authType": "ntlm", "username": "svc", "domain": "CONTOSO"},
			"warehouse": {"database": "SCOMDW"}
		}`)
		if s.Warehouse.Database != "SCOMDW" {
			t.Errorf("warehouse database = %q, want SCOMDW", s.Warehouse.Database)
		}
		if s.Warehouse.Host != "sql01" || s.Warehouse.Username != "svc" || s.Warehouse.Domain != "CONTOSO" || s.Warehouse.AuthType != AuthTypeNTLM {
			t.Errorf("warehouse connection %+v, want the operational server and credentials", s.Warehouse)
		}
		if s.Secrets.WarehousePassword != "secret" {
			t.Error("warehouse password not shared from the operational one")
		}
	})

	t.Run("defaults the database name when none was saved", func(t *testing.T) {
		s := load(`{"warehouseSameAsOperational": true, "operational": {"host": "sql01", "database": "OperationsManager"}}`)
		if s.Warehouse.Database != DefaultWarehouseDatabase {
			t.Errorf("warehouse database = %q, want %q", s.Warehouse.Database, DefaultWarehouseDatabase)
		}
	})

	t.Run("separate warehouse settings are untouched", func(t *testing.T) {
		s := load(`{"operational": {"host": "sql01"}, "warehouse": {"host": "sql02", "database": "OperationsManagerDW"}}`)
		if s.Warehouse.Host != "sql02" {
			t.Errorf("warehouse host = %q, want sql02", s.Warehouse.Host)
		}
	})
}
