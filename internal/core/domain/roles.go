package domain

// PlatformOperatorRoleName is the Keycloak realm-role that grants operator
// access to Commercial platform routes (CD-52). AuthZ Enrichment reads it
// from the access token and injects x-tenant-roles: platform_operator.
// Lives in domain (not the keycloak adapter) so inbound middleware and other
// non-adapter consumers can import it without a transitive gocloak dependency.
const PlatformOperatorRoleName = "platform_operator"
