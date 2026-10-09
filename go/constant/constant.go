// Package constant holds legacy datastore labels for the observability
// package's spans.
//
// Deprecated: datastore/postgres sets the OpenTelemetry semantic-convention
// attributes itself; see MIGRATION.md. This package will be removed in v1.0.
package constant

// DatastoreProduct is used to identify your datastore type for Span with
// Datastore Operation.
type DatastoreProduct string

const (
	DatastoreMySQL    DatastoreProduct = "MySQL"
	DatastorePostgres DatastoreProduct = "PostgresSQL"
)
