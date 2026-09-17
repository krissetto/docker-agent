package leantui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
)

// Plan writes carry the revision explicitly supplied by the user. A stale
// displayed revision must conflict instead of silently replacing newer work.
func (m *model) handlePlans(ctx context.Context, arg string) {
	action, rest, _ := strings.Cut(arg, " ")
	name, rest, _ := strings.Cut(strings.TrimSpace(rest), " ")
	if action == "delete" && !strings.HasSuffix(rest, " confirm") {
		m.reportCapability("Permanently deletes this plan. Use /plans delete <name> <revision> confirm.", nil)
		return
	}
	switch action {
	case "", "list", "view", "create", "edit", "status", "export", "delete":
	default:
		m.reportCapability("Usage: /plans list|view <name>|create <name> <content>|edit <name> <revision> <content>|status <name> <revision> <status>|export <name> <path>|delete <name> <revision> confirm", nil)
		return
	}
	if m.plansService == nil {
		m.plansService = plans.NewService(plan.SharedStorage())
	}
	svc := m.plansService
	ref := plans.SharedRef(name)
	m.capabilityJob(ctx, func(ctx context.Context) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		switch action {
		case "", "list":
			return svc.List(ctx)
		case "view":
			return svc.Get(ctx, ref)
		case "create":
			return svc.Create(ctx, plans.CreateRequest{Ref: ref, Content: rest})
		case "export":
			return svc.Export(ctx, plans.ExportRequest{Ref: ref, Path: rest})
		default:
			versionText, content, _ := strings.Cut(rest, " ")
			version, err := strconv.Atoi(versionText)
			if err != nil || version < 0 {
				return nil, fmt.Errorf("/plans %s requires the exact revision from /plans view <name>", action)
			}
			switch action {
			case "edit":
				return svc.Update(ctx, plans.UpdateRequest{Ref: ref, Content: content, ExpectedVersion: &version})
			case "status":
				return svc.SetStatus(ctx, plans.SetStatusRequest{Ref: ref, Status: content, ExpectedVersion: &version})
			default:
				return "Deleted plan.", svc.Delete(ctx, plans.DeleteRequest{Ref: ref, ExpectedVersion: &version})
			}
		}
	})
}
