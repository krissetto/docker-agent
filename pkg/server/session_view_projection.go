package server

import (
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/labstack/echo/v4"
)

// metadataSessionHeader preserves the fields needed to derive and validate a
// session-view confirmation without serializing history or tree data.
type metadataSessionHeader struct {
	ID         string            `json:"id"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type metadataSubagentAttachInfo struct {
	NodeID          string `json:"NodeID"`
	Agent           string `json:"Agent"`
	Name            string `json:"Name"`
	ParentSessionID string `json:"ParentSessionID"`
	ParentAgent     string `json:"ParentAgent"`
}

type metadataPreparedSessionViewInfo struct {
	ActiveAgentName string                      `json:"active_agent_name,omitempty"`
	SessionID       string                      `json:"session_id"`
	RootSessionID   string                      `json:"root_session_id"`
	RootWorkingDir  string                      `json:"root_working_dir,omitempty"`
	RootBinding     runtime.SessionBinding      `json:"root_binding,omitempty"`
	Session         *metadataSessionHeader      `json:"session"`
	Binding         runtime.SessionBinding      `json:"binding"`
	WorkingDir      string                      `json:"working_dir"`
	Attach          *metadataSubagentAttachInfo `json:"attach,omitempty"`
	Historical      bool                        `json:"historical,omitempty"`
	HistoryReason   string                      `json:"history_reason,omitempty"`
}

func metadataPreparedViewInfo(info runtime.PreparedSessionViewInfo) metadataPreparedSessionViewInfo {
	projected := metadataPreparedSessionViewInfo{
		ActiveAgentName: info.ActiveAgentName,
		SessionID:       info.SessionID,
		RootSessionID:   info.RootSessionID,
		RootWorkingDir:  info.RootWorkingDir,
		RootBinding:     info.RootBinding,
		Binding:         info.Binding,
		WorkingDir:      info.WorkingDir,
		Historical:      info.Historical,
		HistoryReason:   info.HistoryReason,
	}
	if info.Session != nil {
		header := info.Session.MetadataSnapshot()
		projected.Session = &metadataSessionHeader{ID: header.ID, WorkingDir: header.WorkingDir, Attributes: header.AttributesSnapshot()}
	}
	if info.Attach != nil {
		projected.Attach = &metadataSubagentAttachInfo{
			NodeID:          string(info.Attach.NodeID),
			Agent:           info.Attach.Agent,
			Name:            info.Attach.Name,
			ParentSessionID: info.Attach.ParentSessionID,
			ParentAgent:     info.Attach.ParentAgent,
		}
	}
	return projected
}

func sessionViewProjectionError(c echo.Context, expectedView string) (bool, error) {
	values, present := c.QueryParams()["projection"]
	if !present {
		return false, nil
	}
	views, viewPresent := c.QueryParams()["view"]
	if len(values) != 1 || values[0] != api.SessionViewMetadataProjection || !viewPresent || len(views) != 1 || views[0] != expectedView || expectedView == "" {
		return false, sessionRequestError("unsupported session view projection")
	}
	return true, nil
}
