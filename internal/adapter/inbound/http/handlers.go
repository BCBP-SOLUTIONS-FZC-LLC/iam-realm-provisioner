package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handlers is the full RP-1..RP-17 handler surface router.go binds to
// routes. Phase 0/1 wires StubHandlers (returns 501 for every endpoint so
// the service boots and passes health checks before the provisioning
// services land); Phase 3/4 replace it with the real InternalHandler.
type Handlers interface {
	ProvisionTrial(c *gin.Context)            // RP-1
	ProvisionDirect(c *gin.Context)           // RP-2
	Convert(c *gin.Context)                   // RP-3
	RevertConversion(c *gin.Context)          // RP-4
	CreateInvitedUser(c *gin.Context)         // RP-5
	ConfigureIdp(c *gin.Context)              // RP-6
	RemoveIdp(c *gin.Context)                 // RP-7
	ApplyRealmConfig(c *gin.Context)          // RP-8
	ResetMFA(c *gin.Context)                  // RP-9
	Reactivate(c *gin.Context)                // RP-10
	GetRealm(c *gin.Context)                  // RP-11
	ExpireTrial(c *gin.Context)               // RP-12
	PortabilityAudit(c *gin.Context)          // RP-13
	Suspend(c *gin.Context)                   // RP-14
	DeleteUser(c *gin.Context)                // RP-15
	RevokeSessions(c *gin.Context)            // RP-16
	RefreshServiceAccountKeys(c *gin.Context) // RP-17
}

// StubHandlers implements Handlers with a uniform 501 not_implemented
// response — used only until the corresponding phase lands the real logic.
type StubHandlers struct{}

// NewStubHandlers returns a StubHandlers, whose every method responds 501.
func NewStubHandlers() StubHandlers { return StubHandlers{} }

func (StubHandlers) notImplemented(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusNotImplemented, ErrorResponse{Error: "not_implemented", Status: http.StatusNotImplemented})
}

// ProvisionTrial responds 501 (RP-1 not wired to this handler set).
func (s StubHandlers) ProvisionTrial(c *gin.Context) { s.notImplemented(c) }

// ProvisionDirect responds 501 (RP-2 not wired to this handler set).
func (s StubHandlers) ProvisionDirect(c *gin.Context) { s.notImplemented(c) }

// Convert responds 501 (RP-3 not wired to this handler set).
func (s StubHandlers) Convert(c *gin.Context) { s.notImplemented(c) }

// RevertConversion responds 501 (RP-4 not wired to this handler set).
func (s StubHandlers) RevertConversion(c *gin.Context) { s.notImplemented(c) }

// CreateInvitedUser responds 501 (RP-5 not wired to this handler set).
func (s StubHandlers) CreateInvitedUser(c *gin.Context) { s.notImplemented(c) }

// ConfigureIdp responds 501 (RP-6 not wired to this handler set).
func (s StubHandlers) ConfigureIdp(c *gin.Context) { s.notImplemented(c) }

// RemoveIdp responds 501 (RP-7 not wired to this handler set).
func (s StubHandlers) RemoveIdp(c *gin.Context) { s.notImplemented(c) }

// ApplyRealmConfig responds 501 (RP-8 not wired to this handler set).
func (s StubHandlers) ApplyRealmConfig(c *gin.Context) { s.notImplemented(c) }

// ResetMFA responds 501 (RP-9 not wired to this handler set).
func (s StubHandlers) ResetMFA(c *gin.Context) { s.notImplemented(c) }

// Reactivate responds 501 (RP-10 not wired to this handler set).
func (s StubHandlers) Reactivate(c *gin.Context) { s.notImplemented(c) }

// GetRealm responds 501 (RP-11 not wired to this handler set).
func (s StubHandlers) GetRealm(c *gin.Context) { s.notImplemented(c) }

// ExpireTrial responds 501 (RP-12 not wired to this handler set).
func (s StubHandlers) ExpireTrial(c *gin.Context) { s.notImplemented(c) }

// PortabilityAudit responds 501 (RP-13 not wired to this handler set).
func (s StubHandlers) PortabilityAudit(c *gin.Context) { s.notImplemented(c) }

// Suspend responds 501 (RP-14 not wired to this handler set).
func (s StubHandlers) Suspend(c *gin.Context) { s.notImplemented(c) }

// DeleteUser responds 501 (RP-15 not wired to this handler set).
func (s StubHandlers) DeleteUser(c *gin.Context) { s.notImplemented(c) }

// RevokeSessions responds 501 (RP-16 not wired to this handler set).
func (s StubHandlers) RevokeSessions(c *gin.Context) { s.notImplemented(c) }

// RefreshServiceAccountKeys responds 501 (RP-17 not wired to this handler set).
func (s StubHandlers) RefreshServiceAccountKeys(c *gin.Context) { s.notImplemented(c) }

var _ Handlers = StubHandlers{}
