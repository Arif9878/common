// Package utils holds legacy helpers for database/sql null values.
//
// Deprecated: use pgx pointer or pgtype values, or the database/sql Null
// types directly (sql.Null[time.Time] instead of pq.NullTime); see
// MIGRATION.md. This package will be removed in v1.0.
package utils

import (
	"database/sql"
	"time"

	"github.com/lib/pq"
)

// IntOrNull returns properly configured sql.NullInt64
func IntOrNull(n int64) sql.NullInt64 {
	return sql.NullInt64{Int64: n, Valid: true}
}

// PositiveIntOrNull returns properly configured sql.NullInt64 for a positive number
func PositiveIntOrNull(n int64) sql.NullInt64 {
	if n <= 0 {
		return sql.NullInt64{Int64: 0, Valid: false}
	}
	return sql.NullInt64{Int64: n, Valid: true}
}

// FloatOrNull returns properly configured sql.NullFloat64
func FloatOrNull(n float64) sql.NullFloat64 {
	return sql.NullFloat64{Float64: n, Valid: true}
}

// PositiveFloatOrNull returns properly configured sql.NullFloat64 for a positive number
func PositiveFloatOrNull(n float64) sql.NullFloat64 {
	if n <= 0 {
		return sql.NullFloat64{Float64: 0.0, Valid: false}
	}
	return sql.NullFloat64{Float64: n, Valid: true}
}

// StringOrNull returns properly configured sql.NullString
func StringOrNull(str string) sql.NullString {
	if str == "" {
		return sql.NullString{String: "", Valid: false}
	}
	return sql.NullString{String: str, Valid: true}
}

// TimeOrNull returns properly confiigured pq.TimeNull
func TimeOrNull(t *time.Time) pq.NullTime {
	if t == nil {
		return pq.NullTime{Time: time.Time{}, Valid: false}
	}
	return pq.NullTime{Time: *t, Valid: true}
}
