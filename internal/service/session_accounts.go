package service

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

type accountAliasStore interface {
	GetAccountAliases(context.Context) (map[string]string, error)
}

func (b *directBackend) sessionAccountAliases(ctx context.Context) (map[string]string, error) {
	aliases := make(map[string]string)
	if store, ok := b.db.(accountAliasStore); ok {
		remote, err := store.GetAccountAliases(ctx)
		if err != nil {
			return nil, err
		}
		maps.Copy(aliases, remote)
	}
	maps.Copy(aliases, b.accountAliases)
	return aliases, nil
}

func (b *directBackend) resolveAccountFilter(ctx context.Context, filter *string) (map[string]string, error) {
	values := db.CSVFilterValues(*filter)
	if len(values) == 0 {
		return nil, nil
	}
	if _, ok := b.db.(accountAliasStore); !ok {
		return nil, &db.SearchInputError{Msg: "account filtering is unsupported by this store"}
	}
	aliases, err := b.sessionAccountAliases(ctx)
	if err != nil {
		return nil, err
	}
	for i, value := range values {
		if strings.Contains(value, ":") {
			continue
		}
		for id, alias := range aliases {
			if alias == value {
				if values[i] != value {
					return nil, &db.SearchInputError{Msg: fmt.Sprintf("ambiguous account alias %q", value)}
				}
				values[i] = id
			}
		}
	}
	*filter = strings.Join(values, ",")
	return aliases, nil
}

func (b *directBackend) attachContentAccounts(ctx context.Context, matches []db.ContentMatch, aliases map[string]string) error {
	if _, ok := b.db.(accountAliasStore); !ok {
		return nil
	}
	accounts := make(map[string]string)
	var ids []string
	for _, match := range matches {
		switch match.Agent {
		case "chatgpt", "claude-ai", "gemini-apps", "grok-web":
			if _, seen := accounts[match.SessionID]; !seen {
				ids = append(ids, match.SessionID)
				accounts[match.SessionID] = ""
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	page, err := b.db.ListSessions(ctx, db.SessionFilter{IDs: ids, Limit: len(ids)})
	if err != nil {
		return err
	}
	for _, session := range page.Sessions {
		accounts[session.ID] = session.Account
	}
	for i := range matches {
		if account := accounts[matches[i].SessionID]; account != "" {
			if aliases == nil {
				aliases, err = b.sessionAccountAliases(ctx)
				if err != nil {
					return err
				}
			}
			matches[i].Account, matches[i].AccountAlias = account, aliases[account]
		}
	}
	return nil
}
