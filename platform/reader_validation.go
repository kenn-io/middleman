package platform

import (
	"context"
	"fmt"
)

type readerContract struct {
	kind Kind
	host string
}

func (c readerContract) requireRequestedRef(ref RepoRef) error {
	if err := ValidateCanonicalRepoRef(ref); err != nil {
		return c.invalidRequestedRef(err)
	}
	if ref.Platform != c.kind || ref.Host != c.host {
		return c.invalidRequestedRef(fmt.Errorf(
			"repository belongs to %s/%s, not registered provider %s/%s",
			ref.Platform, ref.Host, c.kind, c.host,
		))
	}
	return nil
}

func (c readerContract) invalidRequestedRef(err error) error {
	return &Error{
		Code: ErrCodeInvalidRepoRef, Provider: c.kind, PlatformHost: c.host,
		Field: "repo", Err: err,
	}
}

func (c readerContract) validateItem(
	requested RepoRef,
	returned RepoRef,
	number int,
	itemName string,
) error {
	equal, err := CanonicalRepoRefsEqual(requested, returned)
	if err != nil {
		return ProviderContract(c.kind, c.host, "item_repo", err)
	}
	if !equal {
		return ProviderContract(c.kind, c.host, "item_repo", fmt.Errorf(
			"provider returned repository %s for requested %s",
			returned.DisplayName(), requested.DisplayName(),
		))
	}
	if number <= 0 {
		return ProviderContract(c.kind, c.host, "item_number", fmt.Errorf(
			"provider returned nonpositive %s number %d", itemName, number,
		))
	}
	return nil
}

func validateReaderPage[T any](
	contract readerContract,
	inputCursor string,
	page Page[T],
	validateItem func(T) error,
) error {
	if err := ValidatePage(contract.kind, contract.host, inputCursor, page); err != nil {
		return err
	}
	for _, item := range page.Items {
		if err := validateItem(item); err != nil {
			return err
		}
	}
	return nil
}

type pageReaderValidation struct {
	contract readerContract
	caps     Capabilities
}

func (r pageReaderValidation) prepare(
	ref RepoRef,
	query ItemPageQuery,
	capability ArchiveCapability,
) error {
	if err := r.contract.requireRequestedRef(ref); err != nil {
		return err
	}
	if err := ValidateItemPageQuery(query); err != nil {
		return err
	}
	if query.Order != ItemOrderCreated {
		return nil
	}
	return r.caps.Archive.Require(r.contract.kind, r.contract.host, capability)
}

type validatingIssuePageReader struct {
	reader IssuePageReader
	pageReaderValidation
}

func (r *validatingIssuePageReader) ListIssuesPage(
	ctx context.Context,
	ref RepoRef,
	query ItemPageQuery,
) (Page[Issue], error) {
	if err := r.prepare(ref, query, ArchiveCapabilityHistoricalIssues); err != nil {
		return Page[Issue]{}, err
	}
	page, err := r.reader.ListIssuesPage(ctx, ref, query)
	if err != nil {
		return Page[Issue]{}, err
	}
	err = validateReaderPage(r.contract, query.Cursor, page, func(issue Issue) error {
		return r.contract.validateItem(ref, issue.Repo, issue.Number, "issue")
	})
	return page, err
}

type validatingMergeRequestPageReader struct {
	reader MergeRequestPageReader
	pageReaderValidation
}

func (r *validatingMergeRequestPageReader) ListMergeRequestsPage(
	ctx context.Context,
	ref RepoRef,
	query ItemPageQuery,
) (Page[MergeRequest], error) {
	if err := r.prepare(ref, query, ArchiveCapabilityHistoricalMergeRequests); err != nil {
		return Page[MergeRequest]{}, err
	}
	page, err := r.reader.ListMergeRequestsPage(ctx, ref, query)
	if err != nil {
		return Page[MergeRequest]{}, err
	}
	err = validateReaderPage(r.contract, query.Cursor, page, func(mr MergeRequest) error {
		return r.contract.validateItem(ref, mr.Repo, mr.Number, "merge request")
	})
	return page, err
}
