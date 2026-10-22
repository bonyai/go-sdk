package bonya

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The account calls in this file and in apikeys.go need a login token (see
// WithAPIKey): the service refuses API keys on identity and key management
// with ErrPermissionDenied. An organization the caller is not a member of is
// reported as ErrNotFound; an owner-only call by another role is
// ErrPermissionDenied.

// Role is a member's role in an organization.
type Role string

const (
	// RoleOwner manages the organization, its members and invitations.
	RoleOwner Role = "owner"
	// RoleMember uses sandboxes in the organization.
	RoleMember Role = "member"
	// RoleBilling sees billing only and cannot use sandboxes.
	RoleBilling Role = "billing"
)

// maxOrganizationNameBytes is the service's limit on an organization name.
const maxOrganizationNameBytes = 128

// Organization is an organization the caller belongs to.
type Organization struct {
	ID       string `json:"organization_id"`
	Name     string `json:"name"`
	Personal bool   `json:"personal"`
	// Role is the caller's role; empty when the service did not say.
	Role      Role      `json:"role,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Member is one member of an organization.
type Member struct {
	UserID   string    `json:"user_id"`
	Email    string    `json:"email"`
	Role     Role      `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

// InvitationStatus is where an invitation is in its life.
type InvitationStatus string

const (
	// InvitationLive can still be accepted.
	InvitationLive InvitationStatus = "live"
	// InvitationConsumed was accepted.
	InvitationConsumed InvitationStatus = "consumed"
	// InvitationRevoked was revoked by an owner.
	InvitationRevoked InvitationStatus = "revoked"
	// InvitationExpired passed its expiry unused.
	InvitationExpired InvitationStatus = "expired"
)

// Invitation is an invitation to join an organization.
type Invitation struct {
	ID              string           `json:"invitation_id"`
	OrganizationID  string           `json:"organization_id"`
	Email           string           `json:"email"`
	Role            Role             `json:"role"`
	InvitedByUserID string           `json:"invited_by_user_id"`
	Status          InvitationStatus `json:"status"`
	CreatedAt       time.Time        `json:"created_at"`
	ExpiresAt       time.Time        `json:"expires_at"`
	ConsumedAt      *time.Time       `json:"consumed_at,omitempty"`
	ConsumedBy      string           `json:"consumed_by_user_id,omitempty"`
	RevokedAt       *time.Time       `json:"revoked_at,omitempty"`
}

// CreatedInvitation is a new invitation and its token. The token is returned
// once, here, and never again; send it to the invitee, who passes it to
// AcceptInvitation. String and fmt verbs never print the token.
type CreatedInvitation struct {
	Invitation Invitation
	Token      string
}

// String describes the invitation without its token.
func (c CreatedInvitation) String() string {
	return "invitation " + c.Invitation.ID + " for " + c.Invitation.Email + " (token redacted)"
}

// GoString keeps %#v from printing the token.
func (c CreatedInvitation) GoString() string { return c.String() }

// CreateInvitationOptions describes a new invitation.
type CreateInvitationOptions struct {
	// Email is the invitee's address; only that user can accept.
	Email string
	// Role is granted on acceptance. Defaults to RoleMember.
	Role Role
}

// ListOrganizations returns the organizations the caller belongs to. The
// first call with a new login token also sets up the caller's account and
// personal organization.
func (c *Client) ListOrganizations(ctx context.Context) ([]Organization, error) {
	var out struct {
		Organizations []Organization `json:"organizations"`
	}
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: "/v1/organizations"}, &out); err != nil {
		return nil, err
	}
	return out.Organizations, nil
}

// CreateOrganization creates an organization owned by the caller. It is not
// retried after a lost connection (ErrOutcomeUnknown): list organizations
// before trying again.
func (c *Client) CreateOrganization(ctx context.Context, name string) (*Organization, error) {
	if err := checkOrganizationName(name); err != nil {
		return nil, err
	}
	var out struct {
		Organization Organization `json:"organization"`
	}
	req := &request{method: http.MethodPost, path: "/v1/organizations", json: map[string]string{"name": name}}
	if err := c.doJSON(ctx, req, &out); err != nil {
		return nil, err
	}
	return &out.Organization, nil
}

// GetOrganization returns one organization the caller belongs to.
func (c *Client) GetOrganization(ctx context.Context, organizationID string) (*Organization, error) {
	path, err := organizationPath(organizationID)
	if err != nil {
		return nil, err
	}
	var out struct {
		Organization Organization `json:"organization"`
	}
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: path}, &out); err != nil {
		return nil, err
	}
	return &out.Organization, nil
}

// RenameOrganization changes an organization's name. Owner only.
func (c *Client) RenameOrganization(ctx context.Context, organizationID, name string) (*Organization, error) {
	path, err := organizationPath(organizationID)
	if err != nil {
		return nil, err
	}
	if err := checkOrganizationName(name); err != nil {
		return nil, err
	}
	var out struct {
		Organization Organization `json:"organization"`
	}
	req := &request{method: http.MethodPatch, path: path, json: map[string]string{"name": name}, idempotent: true}
	if err := c.doJSON(ctx, req, &out); err != nil {
		return nil, err
	}
	return &out.Organization, nil
}

// ListMembers returns the members of an organization. Any member may list.
func (c *Client) ListMembers(ctx context.Context, organizationID string) ([]Member, error) {
	path, err := organizationPath(organizationID)
	if err != nil {
		return nil, err
	}
	var out struct {
		Members []Member `json:"members"`
	}
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: path + "/members"}, &out); err != nil {
		return nil, err
	}
	return out.Members, nil
}

// SetMemberRole changes a member's role. Owner only. Demoting the last owner
// fails with ErrConflict (code "final_owner").
func (c *Client) SetMemberRole(ctx context.Context, organizationID, userID string, role Role) (*Member, error) {
	path, err := memberPath(organizationID, userID)
	if err != nil {
		return nil, err
	}
	if err := checkRole(role); err != nil {
		return nil, err
	}
	var out struct {
		Member Member `json:"member"`
	}
	req := &request{method: http.MethodPatch, path: path, json: map[string]string{"role": string(role)}, idempotent: true}
	if err := c.doJSON(ctx, req, &out); err != nil {
		return nil, err
	}
	return &out.Member, nil
}

// RemoveMember removes a member from an organization. Owners may remove
// anyone; any member may remove themselves (leave). Removing the last owner
// fails with ErrConflict (code "final_owner"). A retried removal whose first
// reply was lost may report ErrNotFound.
func (c *Client) RemoveMember(ctx context.Context, organizationID, userID string) error {
	path, err := memberPath(organizationID, userID)
	if err != nil {
		return err
	}
	return c.doJSON(ctx, &request{method: http.MethodDelete, path: path, idempotent: true}, nil)
}

// ListInvitations returns an organization's invitations in every status.
// Owner only.
func (c *Client) ListInvitations(ctx context.Context, organizationID string) ([]Invitation, error) {
	path, err := organizationPath(organizationID)
	if err != nil {
		return nil, err
	}
	var out struct {
		Invitations []Invitation `json:"invitations"`
	}
	if err := c.doJSON(ctx, &request{method: http.MethodGet, path: path + "/invites"}, &out); err != nil {
		return nil, err
	}
	return out.Invitations, nil
}

// CreateInvitation invites an email address into an organization. Owner
// only. A live invitation for the same address fails with ErrConflict;
// revoke it first. The returned token is shown only once.
func (c *Client) CreateInvitation(ctx context.Context, organizationID string, opts CreateInvitationOptions) (*CreatedInvitation, error) {
	path, err := organizationPath(organizationID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Email) == "" {
		return nil, invalidArgument("invitation email is required")
	}
	if opts.Role != "" {
		if err := checkRole(opts.Role); err != nil {
			return nil, err
		}
	}
	body := struct {
		Email string `json:"email"`
		Role  Role   `json:"role,omitempty"`
	}{Email: opts.Email, Role: opts.Role}
	var out struct {
		Invitation Invitation `json:"invitation"`
		Token      string     `json:"token"`
	}
	req := &request{method: http.MethodPost, path: path + "/invites", json: body}
	if err := c.doJSON(ctx, req, &out); err != nil {
		return nil, err
	}
	return &CreatedInvitation{Invitation: out.Invitation, Token: out.Token}, nil
}

// RevokeInvitation revokes an invitation so it can no longer be accepted.
// Owner only. A retried revoke whose first reply was lost may report
// ErrNotFound.
func (c *Client) RevokeInvitation(ctx context.Context, organizationID, invitationID string) error {
	path, err := organizationPath(organizationID)
	if err != nil {
		return err
	}
	if invitationID == "" {
		return invalidArgument("invitation id is required")
	}
	req := &request{method: http.MethodDelete, path: path + "/invites/" + url.PathEscape(invitationID), idempotent: true}
	return c.doJSON(ctx, req, nil)
}

// AcceptInvitation joins the organization an invitation token names and
// returns it. The invitation must have been sent to the caller's own email.
func (c *Client) AcceptInvitation(ctx context.Context, token string) (*Organization, error) {
	if token == "" {
		return nil, invalidArgument("invitation token is required")
	}
	var out struct {
		Organization Organization `json:"organization"`
	}
	req := &request{method: http.MethodPost, path: "/v1/invitations/accept", json: map[string]string{"token": token}}
	if err := c.doJSON(ctx, req, &out); err != nil {
		return nil, err
	}
	return &out.Organization, nil
}

func organizationPath(organizationID string) (string, error) {
	if organizationID == "" {
		return "", invalidArgument("organization id is required")
	}
	return "/v1/organizations/" + url.PathEscape(organizationID), nil
}

func memberPath(organizationID, userID string) (string, error) {
	path, err := organizationPath(organizationID)
	if err != nil {
		return "", err
	}
	if userID == "" {
		return "", invalidArgument("user id is required")
	}
	return path + "/members/" + url.PathEscape(userID), nil
}

func checkOrganizationName(name string) error {
	if name == "" || len(name) > maxOrganizationNameBytes {
		return invalidArgument("organization name is required and must be at most 128 bytes")
	}
	return nil
}

func checkRole(role Role) error {
	switch role {
	case RoleOwner, RoleMember, RoleBilling:
		return nil
	}
	return invalidArgument("role must be owner, member or billing")
}
