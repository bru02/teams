// mautrix-slack - A Matrix-Slack puppeting bridge.
// Copyright (C) 2024 Tulir Asokan
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

package connector

import (
	"strings"

	"github.com/rs/zerolog"
)

type slackgoZerolog struct {
	zerolog.Logger
}

func (l slackgoZerolog) Output(i int, s string) error {
	level := zerolog.DebugLevel
	if strings.HasPrefix(s, "Sending PING ") {
		level = zerolog.TraceLevel
	}
	l.WithLevel(level).Msg(strings.TrimSpace(s))
	return nil
}
