//go:build !windows

package api

import (
	"encoding/json"
	"errors"
)

var errToolsWindowsOnly = errors.New("工具集成仅在 Windows 上可用")

func workBuddyToolStatus() (json.RawMessage, error)       { return nil, errToolsWindowsOnly }
func workBuddyToolClaim() (json.RawMessage, error)        { return nil, errToolsWindowsOnly }
func workBuddyToolRefreshToken() (json.RawMessage, error) { return nil, errToolsWindowsOnly }
func workBuddyToolPanel() error                           { return errToolsWindowsOnly }
func modelScopeToolStatus() (any, error)                  { return nil, errToolsWindowsOnly }
func modelScopeToolAction(action string) (any, error) {
	return nil, errToolsWindowsOnly
}

func (s *Server) setupModelScopeLogin() {}
