package logger

import (
	log "github.com/sirupsen/logrus"
)

// Logger methods interface
//
//go:generate mockery --name ILogger
type ILogger interface {
	getLevel() log.Level
	Debug(args ...interface{})
	Debugf(format string, args ...interface{})
	Info(args ...interface{})
	Infof(format string, args ...interface{})
	Warn(args ...interface{})
	Warnf(format string, args ...interface{})
	Error(args ...interface{})
	Errorf(format string, args ...interface{})
	Panic(args ...interface{})
	Panicf(format string, args ...interface{})
	Fatal(args ...interface{})
	Fatalf(format string, args ...interface{})
	Trace(args ...interface{})
	Tracef(format string, args ...interface{})
}

var (
	Logger ILogger
)

// Application logger
type AppLogger struct {
	Level  string
	Logger *log.Logger
}

// For mapping config logger to email_service logger levels
var loggerLevelMap = map[string]log.Level{
	"debug": log.DebugLevel,
	"info":  log.InfoLevel,
	"warn":  log.WarnLevel,
	"error": log.ErrorLevel,
	"panic": log.PanicLevel,
	"fatal": log.FatalLevel,
	"trace": log.TraceLevel,
}

func (l *AppLogger) GetLevel() log.Level {

	level, exist := loggerLevelMap[l.Level]
	if !exist {
		return log.DebugLevel
	}

	return level
}

func (l *AppLogger) Debug(args ...interface{}) {
	l.Logger.Debug(args...)
}

func (l *AppLogger) Debugf(format string, args ...interface{}) {
	l.Logger.Debugf(format, args...)
}

func (l *AppLogger) Info(args ...interface{}) {
	l.Logger.Info(args...)
}

func (l *AppLogger) Infof(format string, args ...interface{}) {
	l.Logger.Infof(format, args...)
}

func (l *AppLogger) Trace(args ...interface{}) {
	l.Logger.Trace(args...)
}

func (l *AppLogger) Tracef(format string, args ...interface{}) {
	l.Logger.Tracef(format, args...)
}

func (l *AppLogger) Error(args ...interface{}) {
	l.Logger.Error(args...)
}

func (l *AppLogger) Errorf(format string, args ...interface{}) {
	l.Logger.Errorf(format, args...)
}

func (l *AppLogger) Warn(args ...interface{}) {
	l.Logger.Warn(args...)
}

func (l *AppLogger) Warnf(format string, args ...interface{}) {
	l.Logger.Warnf(format, args...)
}

func (l *AppLogger) Panic(args ...interface{}) {
	l.Logger.Panic(args...)
}

func (l *AppLogger) Panicf(format string, args ...interface{}) {
	l.Logger.Panicf(format, args...)
}

func (l *AppLogger) Fatal(args ...interface{}) {
	l.Logger.Fatal(args...)
}

func (l *AppLogger) Fatalf(format string, args ...interface{}) {
	l.Logger.Fatalf(format, args...)
}
