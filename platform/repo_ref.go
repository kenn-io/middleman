package platform

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// ValidateCanonicalRepoRef verifies a provider-normalized repository identity.
// It does not normalize aliases, hosts, or paths because callers use it at
// boundaries where a noncanonical value is itself an error.
func ValidateCanonicalRepoRef(ref RepoRef) error {
	metadata, ok := builtInMetadata[ref.Platform]
	if !ok {
		return invalidCanonicalRepoRef(ref, "unknown or noncanonical platform %q", ref.Platform)
	}
	if err := validateCanonicalRepoHost(ref.Host); err != nil {
		return invalidCanonicalRepoRef(ref, "%v", err)
	}
	if err := validateCanonicalRepoOwner(ref.Owner, metadata.AllowNestedOwner); err != nil {
		return invalidCanonicalRepoRef(ref, "%v", err)
	}
	if err := validateCanonicalRepoName(ref.Name); err != nil {
		return invalidCanonicalRepoRef(ref, "%v", err)
	}
	if ref.RepoPath != "" && ref.RepoPath != ref.Owner+"/"+ref.Name {
		return invalidCanonicalRepoRef(
			ref,
			"repo path %q does not match owner/name identity",
			ref.RepoPath,
		)
	}
	return nil
}

// CanonicalRepoRefsEqual compares the complete provider repository identity
// after verifying that both values are already canonical.
func CanonicalRepoRefsEqual(left, right RepoRef) (bool, error) {
	if err := ValidateCanonicalRepoRef(left); err != nil {
		return false, err
	}
	if err := ValidateCanonicalRepoRef(right); err != nil {
		return false, err
	}
	return left.Platform == right.Platform &&
		left.Host == right.Host &&
		left.Owner == right.Owner &&
		left.Name == right.Name, nil
}

func validateCanonicalRepoHost(host string) error {
	if host == "" || strings.TrimSpace(host) != host {
		return fmt.Errorf("platform host is empty or contains surrounding whitespace")
	}
	if strings.ToLower(host) != host {
		return fmt.Errorf("platform host %q is not lowercase", host)
	}
	if strings.Contains(host, "://") {
		return fmt.Errorf("platform host %q must not include a scheme", host)
	}
	if strings.ContainsFunc(host, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) {
		return fmt.Errorf("platform host %q contains whitespace or control characters", host)
	}
	if strings.HasSuffix(host, ":") {
		return fmt.Errorf("platform host %q has an empty port", host)
	}

	parsed, err := url.Parse("//" + host)
	if err != nil || parsed.User != nil || parsed.Host != host || parsed.Hostname() == "" ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("platform host %q is not a canonical host authority", host)
	}
	if strings.Count(host, ":") > 1 && !strings.HasPrefix(host, "[") {
		return fmt.Errorf("platform host %q must bracket IPv6 literals", host)
	}
	if err := validateCanonicalRepoPort(host); err != nil {
		return err
	}
	if port := parsed.Port(); port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return fmt.Errorf("platform host %q has an invalid port", host)
		}
		if portNumber == 443 {
			return fmt.Errorf("platform host %q contains the noncanonical default TLS port", host)
		}
	}
	return nil
}

func validateCanonicalRepoPort(host string) error {
	port := ""
	if strings.HasPrefix(host, "[") {
		closing := strings.LastIndex(host, "]")
		if closing < 0 {
			return fmt.Errorf("platform host %q is not a canonical host authority", host)
		}
		suffix := host[closing+1:]
		if suffix == "" {
			return nil
		}
		if !strings.HasPrefix(suffix, ":") {
			return fmt.Errorf("platform host %q is not a canonical host authority", host)
		}
		port = strings.TrimPrefix(suffix, ":")
	} else if _, suffix, found := strings.Cut(host, ":"); found {
		port = suffix
	}
	if port == "" {
		return nil
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("platform host %q has an invalid port", host)
	}
	return nil
}

func validateCanonicalRepoOwner(owner string, allowNested bool) error {
	if err := validateCanonicalRepoPathField("owner", owner); err != nil {
		return err
	}
	parts := strings.Split(owner, "/")
	if !allowNested && len(parts) != 1 {
		return fmt.Errorf("owner %q must not be nested for this platform", owner)
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("owner %q contains invalid nesting", owner)
		}
	}
	return nil
}

func validateCanonicalRepoName(name string) error {
	if err := validateCanonicalRepoPathField("name", name); err != nil {
		return err
	}
	if strings.Contains(name, "/") || name == "." || name == ".." {
		return fmt.Errorf("repository name %q contains invalid nesting", name)
	}
	return nil
}

func validateCanonicalRepoPathField(field, value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("repository %s is empty or contains surrounding whitespace", field)
	}
	if strings.ContainsFunc(value, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) {
		return fmt.Errorf("repository %s %q contains whitespace or control characters", field, value)
	}
	return nil
}

func invalidCanonicalRepoRef(ref RepoRef, format string, args ...any) error {
	return &Error{
		Code:         ErrCodeInvalidRepoRef,
		Provider:     ref.Platform,
		PlatformHost: ref.Host,
		Field:        "repo",
		Err:          fmt.Errorf(format, args...),
	}
}
