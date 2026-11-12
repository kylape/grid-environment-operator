package v1alpha1

// Condition types reported in AIGridStatus.Conditions, in reconciliation
// order. Each phase must be True before the next is evaluated.
const (
	// ConditionProvisioned: all LWEs (sites + zone server) exist and are Ready.
	ConditionProvisioned = "Provisioned"

	// ConditionImported: all spoke sites are joined to the hub's ACM.
	ConditionImported = "Imported"

	// ConditionWired: zone server deployed with records, host-side policies
	// applied, guest DNS configured and verified on every site.
	ConditionWired = "Wired"

	// ConditionGitOpsSynced: root Application synced, role labels applied,
	// OperatorGroups annotated for bundle-unpack retries.
	ConditionGitOpsSynced = "GitOpsSynced"

	// ConditionGridReady: every site's GridSite is Connected and the
	// consumer gateway is exposed at IngressEndpointURL.
	ConditionGridReady = "GridReady"
)

// Site phases, mirrored per site in SiteStatus.Phase.
const (
	// SitePhaseProvisioning: the site's LWE is not Ready yet.
	SitePhaseProvisioning = "Provisioning"

	// SitePhaseImporting: the ManagedCluster has not joined yet.
	SitePhaseImporting = "Importing"

	// SitePhaseWired: DNS wiring is complete and verified for this site.
	SitePhaseWired = "Wired"

	// SitePhaseGridReady: the site's GridSite reports Connected.
	SitePhaseGridReady = "GridReady"
)
