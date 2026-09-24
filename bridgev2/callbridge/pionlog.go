// mautrix - A full-featured Matrix client library.
// Copyright (C) 2026 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package callbridge

import (
	"fmt"

	"github.com/pion/logging"
	"github.com/rs/zerolog"
)

// pionLoggerFactory sends Pion's warnings and errors to zerolog; its
// info/debug/trace output is dropped.
type pionLoggerFactory struct{ log zerolog.Logger }

func (f *pionLoggerFactory) NewLogger(scope string) logging.LeveledLogger {
	return &pionLogger{log: f.log.With().Str("pion", scope).Logger()}
}

type pionLogger struct{ log zerolog.Logger }

func (l *pionLogger) Trace(string)                  {}
func (l *pionLogger) Tracef(string, ...any)         {}
func (l *pionLogger) Debug(string)                  {}
func (l *pionLogger) Debugf(string, ...any)         {}
func (l *pionLogger) Info(string)                   {}
func (l *pionLogger) Infof(string, ...any)          {}
func (l *pionLogger) Warn(msg string)               { l.log.Warn().Msg(msg) }
func (l *pionLogger) Warnf(format string, a ...any) { l.log.Warn().Msg(fmt.Sprintf(format, a...)) }
func (l *pionLogger) Error(msg string)              { l.log.Error().Msg(msg) }
func (l *pionLogger) Errorf(format string, a ...any) {
	l.log.Error().Msg(fmt.Sprintf(format, a...))
}
