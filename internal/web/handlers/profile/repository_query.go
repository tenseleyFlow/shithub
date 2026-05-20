// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"strings"

	"github.com/tenseleyFlow/shithub/internal/search"
)

type repositoryQueryCandidate struct {
	Owner       string
	Name        string
	Description string
	Visibility  string
	Language    string
	License     string
	IsFork      bool
	Archived    bool
	Topics      []string
}

func orgRepositoryMatchesParsedQuery(owner string, repo orgProfileRepo, parsed search.ParsedQuery) bool {
	return repositoryMatchesParsedQuery(repositoryQueryCandidate{
		Owner:       owner,
		Name:        repo.Name,
		Description: repo.Description,
		Visibility:  repo.Visibility,
		Language:    repo.PrimaryLanguage,
		License:     repo.LicenseKey,
		IsFork:      repo.IsFork,
		Archived:    repo.Archived,
		Topics:      repo.Topics,
	}, parsed)
}

func userRepositoryMatchesParsedQuery(owner string, repo userRepositoryItem, parsed search.ParsedQuery) bool {
	return repositoryMatchesParsedQuery(repositoryQueryCandidate{
		Owner:       owner,
		Name:        repo.Name,
		Description: repo.Description,
		Visibility:  repo.Visibility,
		Language:    repo.PrimaryLanguage,
		License:     repo.LicenseKey,
		IsFork:      repo.IsFork,
		Archived:    repo.Archived,
		Topics:      repo.Topics,
	}, parsed)
}

func repositoryMatchesParsedQuery(repo repositoryQueryCandidate, parsed search.ParsedQuery) bool {
	if parsed.RepoFilter != nil {
		if !strings.EqualFold(parsed.RepoFilter.Owner, repo.Owner) ||
			!strings.EqualFold(parsed.RepoFilter.Name, repo.Name) {
			return false
		}
	}
	if parsed.OwnerFilter != "" && !strings.EqualFold(parsed.OwnerFilter, repo.Owner) {
		return false
	}
	if parsed.LanguageFilter != "" && !strings.EqualFold(parsed.LanguageFilter, repo.Language) {
		return false
	}
	if parsed.VisibilityFilter != "" && !strings.EqualFold(parsed.VisibilityFilter, repo.Visibility) {
		return false
	}
	if parsed.ForkFilter != nil && repo.IsFork != *parsed.ForkFilter {
		return false
	}
	if parsed.ArchivedFilter != nil && repo.Archived != *parsed.ArchivedFilter {
		return false
	}
	for _, topic := range parsed.TopicFilters {
		if !repositoryHasTopic(repo, topic) {
			return false
		}
	}
	for _, term := range parsed.Terms {
		if !repositoryCandidateContains(repo, term.Value) {
			return false
		}
	}
	for _, term := range parsed.ExcludedTerms {
		if repositoryCandidateContains(repo, term.Value) {
			return false
		}
	}
	return true
}

func repositoryHasTopic(repo repositoryQueryCandidate, want string) bool {
	for _, topic := range repo.Topics {
		if strings.EqualFold(topic, want) {
			return true
		}
	}
	return false
}

func repositoryCandidateContains(repo repositoryQueryCandidate, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	values := []string{
		repo.Owner + "/" + repo.Name,
		repo.Name,
		repo.Description,
		repo.Language,
		repo.License,
		repo.Visibility,
	}
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	for _, topic := range repo.Topics {
		if strings.Contains(strings.ToLower(topic), query) {
			return true
		}
	}
	return false
}
