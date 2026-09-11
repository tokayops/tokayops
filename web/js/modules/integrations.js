/**
 * TokayOps Integrations Module
 * Integration management functionality
 */

import { State } from '/js/core/state.js';
import { Elements, showToast, escapeHtml } from '/js/core/utils.js';
import { ViewManager } from '/js/core/viewManager.js';
import { Permissions } from '/js/modules/permissions.js';

// Module-local delivery state
let modalMode = null; // 'editor' | 'deliveries' | 'delivery-detail'
let deliveries = [];
let deliveryPagination = null;
let currentDeliveryIntegrationId = null;

// One idempotency key per unfinished replay, by delivery. The key is made when
// the button is pressed and lives until the answer proves something: a 2xx or a
// 4xx ends it (done, or refused outright), while a 5xx or no answer at all
// keeps it - the server may have committed and lost the response, and the next
// press must find the same new delivery rather than make a second one. A press
// after an answer is a second decision and gets a new key. The page's memory is
// the boundary: a reload starts over, deliberately.
const replayKeys = new Map();

function newIdempotencyKey() {
    if (window.crypto && typeof window.crypto.randomUUID === 'function') {
        return window.crypto.randomUUID();
    }
    return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}-${Math.random().toString(36).slice(2)}`;
}

/**
 * Parse custom headers text into an object.
 * Returns { ok: true, headers: {} } or { ok: false, error: string }.
 */
function textToHeaders(text) {
    const headers = {};
    if (!text || !text.trim()) return { ok: true, headers };
    const lines = text.split('\n');
    for (let i = 0; i < lines.length; i++) {
        const line = lines[i].trim();
        if (!line) continue;
        const colonIdx = line.indexOf(':');
        if (colonIdx <= 0) {
            return { ok: false, error: `Line ${i + 1}: invalid format (expected "Header-Name: value")` };
        }
        const name = line.substring(0, colonIdx).trim();
        const value = line.substring(colonIdx + 1).trim();
        headers[name] = value;
    }
    return { ok: true, headers };
}

/**
 * Load and render integrations list
 */
export async function loadIntegrations() {
    if (Elements.integrationsLoading) Elements.integrationsLoading.style.display = 'flex';
    if (Elements.integrationsGrid) Elements.integrationsGrid.innerHTML = '';

    try {
        const response = await API.integrations.list();
        State.integrations = response.integrations || [];
        renderIntegrations();
    } catch (error) {
        console.warn('Failed to load integrations:', error);
        showToast('Failed to load integrations', 'error');
    } finally {
        if (Elements.integrationsLoading) Elements.integrationsLoading.style.display = 'none';
    }
}

/**
 * Render integrations grid
 */
function renderIntegrations() {
    if (!Elements.integrationsGrid) return;

    if (State.integrations.length === 0) {
        Elements.integrationsGrid.innerHTML = `
            <div class="empty-state">
                <i data-lucide="plug" class="empty-icon"></i>
                <p>No integrations configured</p>
                <p class="text-sm text-muted">Add your first integration to enable notifications and alerts</p>
            </div>
        `;
    } else {
        Elements.integrationsGrid.innerHTML = State.integrations.map(integration =>
            Components.integrationCard(integration)
        ).join('');
        bindIntegrationCardEvents();
    }

    if (window.lucide) lucide.createIcons();
}

/**
 * Bind events on integration cards
 */
function bindIntegrationCardEvents() {
    if (!Elements.integrationsGrid) return;

    Elements.integrationsGrid.querySelectorAll('.integration-card').forEach(card => {
        card.addEventListener('click', () => {
            const integrationId = card.dataset.integrationId;
            if (integrationId) openIntegrationEditor(integrationId);
        });
    });

    Elements.integrationsGrid.querySelectorAll('.delete-integration-btn').forEach(btn => {
        btn.addEventListener('click', async (e) => {
            e.stopPropagation();
            await handleIntegrationDelete(btn.dataset.integrationId);
        });
    });

    // Deliveries button on generic_webhook cards
    Elements.integrationsGrid.querySelectorAll('.deliveries-btn').forEach(btn => {
        btn.addEventListener('click', (e) => {
            e.stopPropagation();
            openDeliveriesView(btn.dataset.integrationId);
        });
    });
}

/**
 * Show the integrations management view
 */
export function showIntegrationsView() {
    State.currentView = 'integrations';

    // Update sidebar active state
    document.querySelectorAll('.sidebar-nav .nav-item').forEach(nav => nav.classList.remove('active'));
    const integrationsLink = document.querySelector('.nav-item[data-route="integrations"]');
    if (integrationsLink) integrationsLink.classList.add('active');

    ViewManager.show('integrations', { showStats: false, showViewToggle: false });

    loadIntegrations();
}

/**
 * Open integration create/edit modal
 * @param {string|null} integrationId - Integration ID for editing, null for create
 */
export async function openIntegrationEditor(integrationId = null) {
    try {
        let integration = null;
        if (integrationId) {
            integration = await API.integrations.get(integrationId);
        }
        State.editingIntegration = integration;
        modalMode = 'editor';

        // Render modal
        Elements.integrationModalTitle.textContent = integration ? 'Edit Integration' : 'Add Integration';
        Elements.integrationModalBody.innerHTML = Components.integrationFormModal(integration);

        // Render footer with split layout
        const showTestBtn = integration && (integration.type === 'slack' || integration.type === 'generic_webhook') && integration.enabled;
        const testBtnLabel = integration && integration.type === 'generic_webhook' ? 'Test Delivery' : 'Send Test DM';
        const testBtnIcon = integration && integration.type === 'generic_webhook' ? 'webhook' : 'send';
        Elements.integrationModalFooter.innerHTML = `
            <div class="modal-footer-left">
                ${showTestBtn ? `
                    <button type="button" class="btn btn-secondary" id="integration-test-btn" data-integration-id="${integration.id}" data-integration-type="${integration.type}">
                        <i data-lucide="${testBtnIcon}" style="width: 14px; height: 14px; margin-right: 6px;"></i>
                        ${testBtnLabel}
                    </button>
                ` : ''}
            </div>
            <div class="modal-footer-right">
                <button type="button" class="btn btn-secondary" id="integration-form-cancel">Cancel</button>
                <button type="submit" form="integration-form" class="btn btn-primary" id="integration-form-submit">${integration ? 'Save' : 'Create'}</button>
            </div>
        `;
        Elements.integrationModalFooter.classList.add('split');

        Elements.integrationModalOverlay.classList.add('active');
        document.body.style.overflow = 'hidden';

        if (window.lucide) lucide.createIcons();

        // Bind form events
        bindIntegrationEditorEvents();

    } catch (error) {
        showToast('Failed to open integration editor: ' + error.message, 'error');
    }
}


/**
 * Close integration modal and reset all state
 */
export function closeIntegrationModal() {
    if (Elements.integrationModalOverlay) {
        Elements.integrationModalOverlay.classList.remove('active');
        document.body.style.overflow = '';
    }
    if (Elements.integrationModalFooter) {
        Elements.integrationModalFooter.classList.remove('split');
    }
    if (Elements.integrationModalTitle) {
        Elements.integrationModalTitle.textContent = '';
    }
    if (Elements.integrationModalBody) {
        Elements.integrationModalBody.innerHTML = '';
    }
    if (Elements.integrationModalFooter) {
        Elements.integrationModalFooter.innerHTML = '';
    }
    State.editingIntegration = null;
    modalMode = null;
    deliveries = [];
    deliveryPagination = null;
    currentDeliveryIntegrationId = null;
}

/**
 * Bind integration editor form events
 */
function bindIntegrationEditorEvents() {
    const form = document.getElementById('integration-form');
    const typeSelect = document.getElementById('integration-type');
    const cancelBtn = document.getElementById('integration-form-cancel');
    const directionTabs = document.getElementById('direction-tabs');
    const testBtn = document.getElementById('integration-test-btn');

    if (form) {
        form.addEventListener('submit', handleIntegrationSubmit);
    }

    if (typeSelect) {
        typeSelect.addEventListener('change', () => {
            updateConfigFields(typeSelect.value);
            // Show/hide scope section based on type
            updateScopeVisibility(typeSelect.value);
        });
    }

    if (cancelBtn) {
        cancelBtn.addEventListener('click', closeIntegrationModal);
    }

    if (testBtn) {
        testBtn.addEventListener('click', async () => {
            await handleIntegrationTest(testBtn.dataset.integrationId, testBtn.dataset.integrationType);
        });
    }

    // Direction tabs (only for admin create mode)
    if (directionTabs) {
        directionTabs.querySelectorAll('.scope-tab-sm').forEach(tab => {
            tab.addEventListener('click', () => {
                const direction = tab.dataset.direction;

                // Update active state
                directionTabs.querySelectorAll('.scope-tab-sm').forEach(t => t.classList.remove('active'));
                tab.classList.add('active');

                // Update hidden input
                const directionInput = document.getElementById('integration-direction');
                if (directionInput) directionInput.value = direction;

                // Update type select options
                updateTypeOptions(direction);
            });
        });
    }

    // Enabled tabs
    const enabledTabs = document.getElementById('enabled-tabs');
    if (enabledTabs) {
        enabledTabs.querySelectorAll('.scope-tab-sm').forEach(tab => {
            tab.addEventListener('click', () => {
                const enabled = tab.dataset.enabled;

                // Update active state
                enabledTabs.querySelectorAll('.scope-tab-sm').forEach(t => t.classList.remove('active'));
                tab.classList.add('active');

                // Update hidden input
                const enabledInput = document.getElementById('integration-enabled');
                if (enabledInput) enabledInput.value = enabled;
            });
        });
    }

    // Scope tabs (admin create for generic_webhook)
    const scopeTabs = document.getElementById('scope-tabs');
    if (scopeTabs) {
        scopeTabs.querySelectorAll('.scope-tab-sm').forEach(tab => {
            tab.addEventListener('click', () => {
                const scope = tab.dataset.scope;

                scopeTabs.querySelectorAll('.scope-tab-sm').forEach(t => t.classList.remove('active'));
                tab.classList.add('active');

                const scopeInput = document.getElementById('integration-scope');
                if (scopeInput) scopeInput.value = scope;

                const teamGroup = document.getElementById('team-select-group');
                if (teamGroup) {
                    teamGroup.style.display = scope === 'team' ? '' : 'none';
                }
            });
        });
    }

    // Copy URL button - use event delegation on config container
    const configContainer = document.getElementById('integration-config-fields');
    if (configContainer) {
        configContainer.addEventListener('click', (e) => {
            const copyBtn = e.target.closest('.copy-url-btn');
            if (copyBtn) {
                const input = copyBtn.parentElement?.querySelector('input[readonly]');
                const url = input?.value;
                if (url) {
                    navigator.clipboard.writeText(url).then(() => {
                        showToast('URL copied!', 'success');
                    });
                }
            }
        });
    }

    // Get App Manifest button
    const getManifestBtn = document.getElementById('get-manifest-btn');
    if (getManifestBtn) {
        getManifestBtn.addEventListener('click', async () => {
            const manifestContent = document.getElementById('manifest-content');
            const manifestYaml = document.getElementById('manifest-yaml');
            if (!manifestContent || !manifestYaml) return;

            if (manifestContent.style.display !== 'none') {
                manifestContent.style.display = 'none';
                return;
            }

            if (!manifestYaml.value) {
                try {
                    getManifestBtn.disabled = true;
                    const manifest = await API.integrations.slackManifest();
                    manifestYaml.value = manifest;
                } catch (error) {
                    showToast('Failed to load manifest: ' + error.message, 'error');
                    return;
                } finally {
                    getManifestBtn.disabled = false;
                }
            }

            manifestContent.style.display = 'block';
            if (window.lucide) lucide.createIcons();
        });
    }

    // Copy manifest button
    const copyManifestBtn = document.getElementById('copy-manifest-btn');
    if (copyManifestBtn) {
        copyManifestBtn.addEventListener('click', () => {
            const manifestYaml = document.getElementById('manifest-yaml');
            if (manifestYaml) {
                navigator.clipboard.writeText(manifestYaml.value)
                    .then(() => showToast('Manifest copied!', 'success'));
            }
        });
    }
}

/**
 * Show/hide scope section based on integration type
 */
function updateScopeVisibility(type) {
    const scopeSection = document.getElementById('scope-section');
    if (!scopeSection) return;
    scopeSection.style.display = type === 'generic_webhook' ? '' : 'none';
}

/**
 * Update type options based on direction
 */
function updateTypeOptions(direction) {
    const typeSelect = document.getElementById('integration-type');
    if (!typeSelect) return;

    const isAdmin = Permissions.isAdmin();
    const typesByDirection = {
        outbound: isAdmin
            ? [{ value: 'slack', label: 'Slack' }, { value: 'telegram', label: 'Telegram' }, { value: 'generic_webhook', label: 'Generic Webhook' }]
            : [{ value: 'generic_webhook', label: 'Generic Webhook' }],
        inbound: [{ value: 'alertmanager_webhook', label: 'Alertmanager Webhook' }]
    };

    const types = typesByDirection[direction] || [];
    typeSelect.innerHTML = types.map(t => `<option value="${t.value}">${t.label}</option>`).join('');

    // Update config fields for new type
    if (types.length > 0) {
        updateConfigFields(types[0].value);
        updateScopeVisibility(types[0].value);
    }
}

/**
 * Update config fields based on integration type
 */
function updateConfigFields(type) {
    const configContainer = document.getElementById('integration-config-fields');
    if (!configContainer) return;

    configContainer.innerHTML = Components.integrationConfigFields(type, null);

    // Update name placeholder based on type
    const nameInput = document.getElementById('integration-name');
    if (nameInput) {
        const placeholders = { slack: 'e.g., Production Slack', telegram: 'e.g., Production Telegram', generic_webhook: 'e.g., Prod Webhook', alertmanager_webhook: 'e.g., Prod Alertmanager' };
        nameInput.placeholder = placeholders[type] || 'e.g., My Integration';
    }

    // Show/hide manifest button based on type
    const manifestBtn = document.getElementById('get-manifest-btn');
    if (manifestBtn) {
        manifestBtn.style.display = type === 'slack' ? 'inline-flex' : 'none';
        const manifestContent = document.getElementById('manifest-content');
        if (manifestContent) manifestContent.style.display = 'none';
    }

    if (window.lucide) lucide.createIcons();
}

/**
 * Handle integration form submission
 */
async function handleIntegrationSubmit(e) {
    e.preventDefault();

    const type = document.getElementById('integration-type')?.value;
    const name = document.getElementById('integration-name')?.value?.trim();
    const enabledInput = document.getElementById('integration-enabled');
    const enabled = enabledInput?.value === 'true';

    if (!name) {
        showToast('Name is required', 'error');
        return;
    }

    // Build config based on type
    let config = {};
    if (type === 'slack') {
        const token = document.getElementById('config-token')?.value?.trim() || '';
        const userToken = document.getElementById('config-user-token')?.value?.trim() || '';
        const defaultChannel = document.getElementById('config-default-channel')?.value?.trim() || '';
        const signingSecret = document.getElementById('config-signing-secret')?.value?.trim() || '';
        const interactive = document.getElementById('config-interactive')?.checked || false;

        config = { token, user_token: userToken, default_channel: defaultChannel, signing_secret: signingSecret, interactive };

        if (!State.editingIntegration && !token) {
            showToast('Bot token is required', 'error');
            return;
        }
    } else if (type === 'telegram') {
        const botToken = document.getElementById('config-bot-token')?.value?.trim() || '';
        const secretToken = document.getElementById('config-secret-token')?.value?.trim() || '';
        const defaultChatID = document.getElementById('config-default-chat-id')?.value?.trim() || '';
        const interactive = document.getElementById('config-interactive')?.checked || false;

        config = { bot_token: botToken, secret_token: secretToken, default_chat_id: defaultChatID, interactive };

        if (!State.editingIntegration && !botToken) {
            showToast('Bot token is required', 'error');
            return;
        }
        if (!State.editingIntegration && !secretToken) {
            showToast('Secret token is required (the webhook needs it for account linking and Ack/Resolve buttons)', 'error');
            return;
        }
    } else if (type === 'alertmanager_webhook') {
        const secret = document.getElementById('config-secret')?.value?.trim() || '';
        config = { secret };

        if (!State.editingIntegration && !secret) {
            showToast('Webhook secret is required', 'error');
            return;
        }
    } else if (type === 'generic_webhook') {
        const url = document.getElementById('config-webhook-url')?.value?.trim() || '';
        const secret = document.getElementById('config-webhook-secret')?.value?.trim() || '';
        const timeoutStr = document.getElementById('config-webhook-timeout')?.value?.trim() || '';
        const headersText = document.getElementById('config-webhook-headers')?.value || '';

        if (!url) {
            showToast('Webhook URL is required', 'error');
            return;
        }
        const headersParsed = textToHeaders(headersText);
        if (!headersParsed.ok) {
            showToast('Custom headers: ' + headersParsed.error, 'error');
            return;
        }

        config = { url, secret };
        if (timeoutStr) config.timeout_seconds = parseInt(timeoutStr, 10);
        if (Object.keys(headersParsed.headers).length > 0) config.custom_headers = headersParsed.headers;
    }

    const data = { name, enabled, config };
    if (!State.editingIntegration) {
        data.type = type;

        // Add scope/team_id for generic_webhook
        if (type === 'generic_webhook') {
            const scope = document.getElementById('integration-scope')?.value || '';
            const teamId = document.getElementById('integration-team-id')?.value || '';

            data.scope = scope;

            if (scope === 'team') {
                if (!teamId) {
                    showToast('Please select a team', 'error');
                    return;
                }
                data.team_id = teamId;
            }
            // scope=global → no team_id (server default)
        }
    }

    const saveBtn = document.getElementById('integration-form-submit');
    if (saveBtn) {
        saveBtn.disabled = true;
        saveBtn.textContent = 'Saving...';
    }

    try {
        if (State.editingIntegration) {
            await API.integrations.update(State.editingIntegration.id, data);
            showToast('Integration updated', 'success');
        } else {
            await API.integrations.create(data);
            showToast('Integration created', 'success');
        }
        closeIntegrationModal();
        loadIntegrations();
    } catch (error) {
        showToast('Failed to save integration: ' + error.message, 'error');
    } finally {
        if (saveBtn) {
            saveBtn.disabled = false;
            saveBtn.textContent = 'Save';
        }
    }
}

/**
 * Handle integration delete
 */
async function handleIntegrationDelete(integrationId) {
    if (!confirm('Delete this integration? This cannot be undone.')) return;

    try {
        await API.integrations.delete(integrationId);
        showToast('Integration deleted', 'success');
        loadIntegrations();
    } catch (error) {
        showToast('Failed to delete integration: ' + error.message, 'error');
    }
}

/**
 * Handle integration test (send test DM for Slack, test delivery for webhook)
 */
async function handleIntegrationTest(integrationId, integrationType) {
    try {
        const mode = integrationType === 'generic_webhook' ? 'delivery' : 'dm';
        const result = await API.integrations.test(integrationId, { mode });
        if (result.ok) {
            showToast(result.message || 'Test sent', 'success');
        } else {
            showToast(result.message || 'Test failed', 'error');
        }
    } catch (error) {
        if (error.message === 'link your Slack account first') {
            showToast('Link your Slack account first', 'error');
        } else {
            showToast('Test failed: ' + error.message, 'error');
        }
    }
}

// ========================================
// Delivery Views
// ========================================

/**
 * Open deliveries list view in the modal
 */
async function openDeliveriesView(integrationId) {
    try {
        currentDeliveryIntegrationId = integrationId;
        modalMode = 'deliveries';

        // Get integration name
        const integration = State.integrations.find(i => i.id === integrationId);
        const integrationName = integration ? integration.name : integrationId;

        Elements.integrationModalTitle.textContent = `Deliveries — ${integrationName}`;
        Elements.integrationModalBody.innerHTML = '<div style="padding: var(--space-lg); text-align: center; color: var(--text-muted);">Loading...</div>';
        Elements.integrationModalFooter.innerHTML = `
            <div class="modal-footer-right">
                <button type="button" class="btn btn-secondary" id="delivery-close-btn">Close</button>
            </div>
        `;
        Elements.integrationModalFooter.classList.remove('split');

        Elements.integrationModalOverlay.classList.add('active');
        document.body.style.overflow = 'hidden';

        document.getElementById('delivery-close-btn')?.addEventListener('click', closeIntegrationModal);

        await loadDeliveryPage(integrationId, 1);
    } catch (error) {
        showToast('Failed to load deliveries: ' + error.message, 'error');
    }
}

/**
 * Load a page of deliveries and render
 */
async function loadDeliveryPage(integrationId, page) {
    try {
        const response = await API.integrations.deliveries(integrationId, { page, limit: 20 });
        deliveries = response.deliveries || [];
        deliveryPagination = {
            page: response.page || page,
            total_pages: response.total_pages || 1,
            total: response.total || deliveries.length
        };

        Elements.integrationModalBody.innerHTML = Components.deliveryListPanel(deliveries, deliveryPagination, integrationId);
        if (window.lucide) lucide.createIcons();

        bindDeliveryListEvents(integrationId);
    } catch (error) {
        Elements.integrationModalBody.innerHTML = `<div style="padding: var(--space-lg); text-align: center; color: var(--text-muted);">Failed to load deliveries: ${escapeHtml(error.message)}</div>`;
    }
}

/**
 * Open delivery detail view
 */
async function openDeliveryDetail(integrationId, deliveryId) {
    try {
        modalMode = 'delivery-detail';

        Elements.integrationModalBody.innerHTML = '<div style="padding: var(--space-lg); text-align: center; color: var(--text-muted);">Loading...</div>';

        const response = await API.integrations.deliveryDetail(integrationId, deliveryId);
        const delivery = response.delivery;
        const attempts = response.attempts || [];
        const isTerminal = delivery.status === 'sent' || delivery.status === 'failed';

        Elements.integrationModalBody.innerHTML = Components.deliveryDetailPanel(delivery, attempts, integrationId);

        Elements.integrationModalFooter.innerHTML = `
            <div class="modal-footer-left">
                <button type="button" class="btn btn-secondary" id="delivery-back-btn">
                    <i data-lucide="arrow-left" style="width: 14px; height: 14px; margin-right: 6px;"></i>
                    Back to list
                </button>
            </div>
            <div class="modal-footer-right">
                ${isTerminal ? `
                    <button type="button" class="btn btn-secondary" id="delivery-detail-replay-btn" data-delivery-id="${escapeHtml(deliveryId)}" data-integration-id="${escapeHtml(integrationId)}">
                        <i data-lucide="rotate-ccw" style="width: 14px; height: 14px; margin-right: 6px;"></i>
                        Replay
                    </button>
                ` : ''}
                <button type="button" class="btn btn-secondary" id="delivery-close-btn">Close</button>
            </div>
        `;
        Elements.integrationModalFooter.classList.add('split');

        if (window.lucide) lucide.createIcons();

        document.getElementById('delivery-back-btn')?.addEventListener('click', () => {
            openDeliveriesViewFromDetail(integrationId);
        });
        document.getElementById('delivery-close-btn')?.addEventListener('click', closeIntegrationModal);
        document.getElementById('delivery-detail-replay-btn')?.addEventListener('click', async (e) => {
            await handleReplayDelivery(e.target.closest('[data-integration-id]').dataset.integrationId, e.target.closest('[data-delivery-id]').dataset.deliveryId);
        });

    } catch (error) {
        Elements.integrationModalBody.innerHTML = `<div style="padding: var(--space-lg); text-align: center; color: var(--text-muted);">Failed to load delivery: ${escapeHtml(error.message)}</div>`;
    }
}

/**
 * Return to deliveries list from detail view
 */
async function openDeliveriesViewFromDetail(integrationId) {
    modalMode = 'deliveries';

    const integration = State.integrations.find(i => i.id === integrationId);
    const integrationName = integration ? integration.name : integrationId;
    Elements.integrationModalTitle.textContent = `Deliveries — ${integrationName}`;

    Elements.integrationModalFooter.innerHTML = `
        <div class="modal-footer-right">
            <button type="button" class="btn btn-secondary" id="delivery-close-btn">Close</button>
        </div>
    `;
    Elements.integrationModalFooter.classList.remove('split');
    document.getElementById('delivery-close-btn')?.addEventListener('click', closeIntegrationModal);

    const page = deliveryPagination?.page || 1;
    await loadDeliveryPage(integrationId, page);
}

/**
 * Handle replay delivery: one request per press, the button held while it is
 * in flight, and the NEW delivery opened when it succeeds - the original stays
 * exactly as it was, so reopening it would show a screen that never changes.
 */
async function handleReplayDelivery(integrationId, deliveryId) {
    const slot = `${integrationId}/${deliveryId}`;
    if (!replayKeys.has(slot)) replayKeys.set(slot, newIdempotencyKey());
    const key = replayKeys.get(slot);

    const buttons = Array.from(document.querySelectorAll('.replay-btn, #delivery-detail-replay-btn'))
        .filter(btn => btn.dataset.deliveryId === deliveryId);
    buttons.forEach(btn => { btn.disabled = true; });
    try {
        const result = await API.integrations.replayDelivery(integrationId, deliveryId, key);
        replayKeys.delete(slot);
        showToast('Delivery queued for replay', 'success');

        const newDeliveryId = result?.delivery_id || deliveryId;
        await openDeliveryDetail(integrationId, newDeliveryId);
        // Once more after the worker has had its turn, if the screen is still
        // on this delivery.
        setTimeout(async () => {
            if (modalMode === 'delivery-detail' && currentDeliveryIntegrationId === integrationId) {
                await openDeliveryDetail(integrationId, newDeliveryId);
            }
        }, 3000);
    } catch (error) {
        // A 4xx is the server refusing the request itself: nothing was made,
        // and the same key would fail the same way. Anything else leaves the
        // key for the next press.
        if (error.status >= 400 && error.status < 500) replayKeys.delete(slot);
        showToast('Replay failed: ' + error.message, 'error');
    } finally {
        buttons.forEach(btn => { btn.disabled = false; });
    }
}

/**
 * Bind events on delivery list (pagination, row click, replay)
 */
function bindDeliveryListEvents(integrationId) {
    const body = Elements.integrationModalBody;
    if (!body) return;

    // Row click → detail
    body.querySelectorAll('.delivery-table tbody tr').forEach(row => {
        row.addEventListener('click', (e) => {
            // Don't navigate if clicking a button
            if (e.target.closest('button')) return;
            const deliveryId = row.dataset.deliveryId;
            if (deliveryId) openDeliveryDetail(integrationId, deliveryId);
        });
    });

    // Replay buttons
    body.querySelectorAll('.replay-btn').forEach(btn => {
        btn.addEventListener('click', async (e) => {
            e.stopPropagation();
            await handleReplayDelivery(btn.dataset.integrationId, btn.dataset.deliveryId);
        });
    });

    // Pagination
    const prevBtn = body.querySelector('.delivery-prev-btn');
    const nextBtn = body.querySelector('.delivery-next-btn');
    if (prevBtn) {
        prevBtn.addEventListener('click', () => {
            const page = (deliveryPagination?.page || 1) - 1;
            if (page >= 1) loadDeliveryPage(integrationId, page);
        });
    }
    if (nextBtn) {
        nextBtn.addEventListener('click', () => {
            const page = (deliveryPagination?.page || 1) + 1;
            if (page <= (deliveryPagination?.total_pages || 1)) loadDeliveryPage(integrationId, page);
        });
    }
}

/**
 * Bind integration-related event listeners
 */
export function bindIntegrationsEvents() {
    // Create button
    if (Elements.createIntegrationBtn) {
        Elements.createIntegrationBtn.addEventListener('click', () => openIntegrationEditor());
    }

    // Modal close button
    if (Elements.integrationModalClose) {
        Elements.integrationModalClose.addEventListener('click', closeIntegrationModal);
    }

    // Modal overlay click to close
    if (Elements.integrationModalOverlay) {
        Elements.integrationModalOverlay.addEventListener('click', (e) => {
            if (e.target === Elements.integrationModalOverlay) {
                closeIntegrationModal();
            }
        });
    }
}
