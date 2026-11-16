package api

import (
	"net/http"

	"github.com/Bazsi-2006/ScryptServerManagger/internal/audit"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/manager"
	"github.com/Bazsi-2006/ScryptServerManagger/internal/perm"
)

// Handler builds the complete HTTP handler. All API routes live under
// /api/v1; everything else serves the embedded frontend.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	const v = "/api/v1"

	// Unauthenticated.
	mux.HandleFunc("GET "+v+"/health", s.public(s.handleHealth))
	mux.HandleFunc("GET "+v+"/meta", s.public(s.handleMeta))
	mux.HandleFunc("GET "+v+"/openapi.yaml", s.public(s.handleOpenAPI))
	mux.HandleFunc("POST "+v+"/auth/login", s.public(s.handleLogin))
	mux.HandleFunc("POST "+v+"/auth/refresh", s.public(s.handleRefresh))
	mux.HandleFunc("POST "+v+"/auth/logout", s.public(s.handleLogout))
	mux.HandleFunc("POST "+v+"/auth/invitation", s.public(s.handleInvitationCheck))
	mux.HandleFunc("POST "+v+"/auth/register", s.public(s.handleRegister))
	mux.HandleFunc("POST "+v+"/auth/email/verify", s.public(s.handleVerifyEmail))
	mux.HandleFunc("GET "+v+"/downloads/{ticket}", s.public(s.handleDownload))

	// Profile.
	mux.HandleFunc("GET "+v+"/me", s.authed(s.handleMe))
	mux.HandleFunc("PATCH "+v+"/me", s.authed(s.handleUpdateMe))
	mux.HandleFunc("POST "+v+"/me/password", s.authed(s.handleChangePassword))
	mux.HandleFunc("POST "+v+"/me/email", s.authed(s.handleRequestEmailChange))
	mux.HandleFunc("GET "+v+"/me/sessions", s.authed(s.handleMySessions))
	mux.HandleFunc("DELETE "+v+"/me/sessions/{sid}", s.authed(s.handleRevokeMySession))
	mux.HandleFunc("POST "+v+"/me/sessions/revoke-others", s.authed(s.handleRevokeOtherSessions))

	// Real-time.
	mux.HandleFunc("GET "+v+"/events", s.authed(s.handleEvents))
	mux.HandleFunc("GET "+v+"/system/metrics", s.authed(s.handleMetrics))

	// Servers. Per-server authorization happens in each handler (access).
	mux.HandleFunc("GET "+v+"/servers", s.authed(s.handleListServers))
	mux.HandleFunc("POST "+v+"/servers", s.admin(s.handleCreateServer))
	mux.HandleFunc("GET "+v+"/servers/{id}", s.authed(s.handleGetServer))
	mux.HandleFunc("DELETE "+v+"/servers/{id}", s.admin(s.handleDeleteServer))
	mux.HandleFunc("GET "+v+"/servers/{id}/config", s.authed(s.handleGetConfig))
	mux.HandleFunc("PUT "+v+"/servers/{id}/config", s.authed(s.handlePutConfig))
	mux.HandleFunc("GET "+v+"/servers/{id}/status", s.authed(s.handleStatus))
	mux.HandleFunc("GET "+v+"/servers/{id}/operations", s.authed(s.handleOperations))
	mux.HandleFunc("POST "+v+"/servers/{id}/start", s.authed(s.operate(manager.OpStart, perm.Start, audit.ServerStart)))
	mux.HandleFunc("POST "+v+"/servers/{id}/stop", s.authed(s.operate(manager.OpStop, perm.Stop, audit.ServerStop)))
	mux.HandleFunc("POST "+v+"/servers/{id}/restart", s.authed(s.operate(manager.OpRestart, perm.Stop, audit.ServerRestart)))
	mux.HandleFunc("GET "+v+"/servers/{id}/console", s.authed(s.handleConsoleSnapshot))
	mux.HandleFunc("GET "+v+"/servers/{id}/console/stream", s.authed(s.handleConsoleStream))
	mux.HandleFunc("GET "+v+"/servers/{id}/console/history", s.authed(s.handleConsoleHistory))
	mux.HandleFunc("POST "+v+"/servers/{id}/console/command", s.authed(s.handleConsoleCommand))
	mux.HandleFunc("GET "+v+"/servers/{id}/backups", s.authed(s.handleListBackups))
	mux.HandleFunc("POST "+v+"/servers/{id}/backups", s.authed(s.handleCreateBackup))
	mux.HandleFunc("POST "+v+"/servers/{id}/backups/cancel", s.authed(s.handleCancelBackup))
	mux.HandleFunc("DELETE "+v+"/servers/{id}/backups/{name}", s.authed(s.handleDeleteBackup))
	mux.HandleFunc("POST "+v+"/servers/{id}/backups/{name}/download", s.authed(s.handleDownloadTicket))
	mux.HandleFunc("GET "+v+"/servers/{id}/permissions", s.admin(s.handleServerPermissions))

	// Administration.
	mux.HandleFunc("GET "+v+"/admin/users", s.admin(s.handleListUsers))
	mux.HandleFunc("GET "+v+"/admin/users/{uid}", s.admin(s.handleGetUser))
	mux.HandleFunc("PATCH "+v+"/admin/users/{uid}", s.admin(s.handleUpdateUser))
	mux.HandleFunc("DELETE "+v+"/admin/users/{uid}", s.admin(s.handleDeleteUser))
	mux.HandleFunc("PUT "+v+"/admin/users/{uid}/permissions/{id}", s.admin(s.handleSetPermission))
	mux.HandleFunc("DELETE "+v+"/admin/users/{uid}/sessions", s.admin(s.handleRevokeUserSessions))
	mux.HandleFunc("DELETE "+v+"/admin/users/{uid}/sessions/{sid}", s.admin(s.handleRevokeUserSessions))
	mux.HandleFunc("GET "+v+"/admin/invitations", s.admin(s.handleListInvitations))
	mux.HandleFunc("POST "+v+"/admin/invitations", s.admin(s.handleCreateInvitation))
	mux.HandleFunc("DELETE "+v+"/admin/invitations/{iid}", s.admin(s.handleRevokeInvitation))
	mux.HandleFunc("POST "+v+"/admin/invitations/{iid}/resend", s.admin(s.handleResendInvitation))
	mux.HandleFunc("GET "+v+"/admin/audit", s.admin(s.handleAudit))
	mux.HandleFunc("GET "+v+"/admin/settings", s.admin(s.handleGetSettings))
	mux.HandleFunc("PUT "+v+"/admin/settings", s.admin(s.handlePutSettings))
	mux.HandleFunc("POST "+v+"/admin/settings/test-email", s.admin(s.handleTestEmail))
	mux.HandleFunc("POST "+v+"/admin/servers/reload", s.admin(s.handleReloadServers))

	mux.HandleFunc("/", s.handleStatic)
	return s.base(mux)
}
