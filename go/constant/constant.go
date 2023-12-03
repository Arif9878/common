package constant

// DatastoreProduct is used to identify your datastore type for Span with
// Datastore Operation.
type DatastoreProduct string

const (
	DatastoreMySQL    DatastoreProduct = "MySQL"
	DatastorePostgres DatastoreProduct = "PostgresSQL"
)
