/**
 * Profile Module
 * Handles user profile settings and API token management
 */

const ProfileModule = {
    currentUser: null,
    tokens: [],
    newlyCreatedToken: null,
    _staticListenersAttached: false,
    otpSent: false, // Track if OTP flow is active
    phone: null, // GET /me/phone, or null when phone calls are not configured
    phoneEditing: false, // the number is being changed

    /**
     * Initialize the profile module
     */
    init() {
        this.setupStaticListeners();
    },

    /**
     * Setup static event listeners (called once)
     */
    setupStaticListeners() {
        if (this._staticListenersAttached) return;
        this._staticListenersAttached = true;

        // Profile modal close button
        const closeBtn = document.getElementById('profile-modal-close');
        if (closeBtn) {
            closeBtn.addEventListener('click', () => this.closeModal());
        }

        // Close on overlay click
        const overlay = document.getElementById('profile-modal-overlay');
        if (overlay) {
            overlay.addEventListener('click', (e) => {
                if (e.target === overlay) this.closeModal();
            });
        }
    },

    /**
     * Setup dynamic event listeners (called after each render)
     */
    setupDynamicListeners() {
        // Profile form submit
        const form = document.getElementById('profile-form');
        if (form) {
            form.addEventListener('submit', (e) => this.handleProfileSubmit(e));
        }

        // Token form submit
        const tokenForm = document.getElementById('token-form');
        if (tokenForm) {
            tokenForm.addEventListener('submit', (e) => this.handleTokenCreate(e));
        }

        // Slack Bind Actions
        const requestOtpBtn = document.getElementById('slack-request-otp-btn');
        if (requestOtpBtn) {
            requestOtpBtn.addEventListener('click', () => this.handleRequestOTP());
        }

        const confirmOtpBtn = document.getElementById('slack-confirm-otp-btn');
        if (confirmOtpBtn) {
            confirmOtpBtn.addEventListener('click', () => this.handleConfirmOTP());
        }

        const cancelOtpBtn = document.getElementById('slack-cancel-otp-btn');
        if (cancelOtpBtn) {
            cancelOtpBtn.addEventListener('click', () => this.handleCancelOTP());
        }

        const unbindBtn = document.getElementById('slack-unbind-btn');
        if (unbindBtn) {
            unbindBtn.addEventListener('click', () => this.handleUnbindSlack());
        }

        // Telegram Bind Actions (deep-link, not OTP)
        const tgConnectBtn = document.getElementById('telegram-connect-btn');
        if (tgConnectBtn) {
            tgConnectBtn.addEventListener('click', () => this.handleRequestTelegramLink());
        }
        const tgRefreshBtn = document.getElementById('telegram-refresh-btn');
        if (tgRefreshBtn) {
            tgRefreshBtn.addEventListener('click', () => this.handleRefreshTelegram());
        }
        const tgUnbindBtn = document.getElementById('telegram-unbind-btn');
        if (tgUnbindBtn) {
            tgUnbindBtn.addEventListener('click', () => this.handleUnbindTelegram());
        }

        // Phone Actions
        const on = (id, handler) => {
            const el = document.getElementById(id);
            if (el) el.addEventListener('click', handler);
        };
        on('phone-save-btn', () => this.handleSavePhone());
        on('phone-edit-btn', () => { this.phoneEditing = true; this.renderModal(); });
        on('phone-cancel-edit-btn', () => { this.phoneEditing = false; this.renderModal(); });
        on('phone-remove-btn', () => this.handleRemovePhone());
        on('phone-call-code-btn', () => this.handleCallWithCode());
        on('phone-confirm-code-btn', () => this.handleConfirmPhoneCode());
        const pinSelect = document.getElementById('phone-pin-select');
        if (pinSelect) {
            pinSelect.addEventListener('change', () => this.handlePinPhone(pinSelect.value));
        }
        document.querySelectorAll('[data-phone-test]').forEach(btn => {
            btn.addEventListener('click', () => this.handlePhoneTestCall(btn.dataset.phoneTest, btn));
        });
        document.querySelectorAll('[data-phone-rang]').forEach(btn => {
            btn.addEventListener('click', () => this.handlePhoneRang(btn.dataset.phoneRang, btn));
        });
    },

    /**
     * Open the profile modal
     */
    async openModal() {
        try {
            // Reset OTP state on open
            this.otpSent = false;

            // Fetch current user data
            this.currentUser = await API.auth.me();

            // Fetch user's tokens
            const tokensResp = await API.tokens.list();
            this.tokens = tokensResp.tokens || [];

            // The phone section shows only where phone calls are configured.
            this.phoneEditing = false;
            try {
                this.phone = await API.auth.phone.get();
            } catch (error) {
                this.phone = null;
            }

            // Render modal content
            this.renderModal();

            // Render footer
            const footer = document.getElementById('profile-modal-footer');
            if (footer) {
                footer.innerHTML = `<button type="button" class="btn btn-secondary" id="profile-close-btn">Close</button>`;
                const closeBtn = document.getElementById('profile-close-btn');
                if (closeBtn) {
                    closeBtn.addEventListener('click', () => this.closeModal());
                }
            }

            // Show modal
            document.getElementById('profile-modal-overlay').classList.add('active');
            document.body.style.overflow = 'hidden';
        } catch (error) {
            console.error('Error opening profile modal:', error);
            if (window.showToast) {
                window.showToast('Failed to load profile', 'error');
            }
        }
    },

    /**
     * Close the profile modal
     */
    closeModal() {
        document.getElementById('profile-modal-overlay').classList.remove('active');
        document.body.style.overflow = '';
        this.newlyCreatedToken = null;
        this.otpSent = false;
    },

    /**
     * Render the profile modal content
     */
    renderModal() {
        const body = document.getElementById('profile-modal-body');
        if (!body) return;

        const isSSO = this.currentUser.auth_provider === 'oidc';

        body.innerHTML = `
            <div class="profile-content">
                <!-- Profile Section -->
                <div class="profile-section">
                    <h3 class="profile-section-title">
                        <i data-lucide="user" class="section-icon"></i>
                        Profile Information
                    </h3>
                    <form id="profile-form" class="profile-form">
                        <div class="form-group">
                            <label for="profile-name">Name ${isSSO ? '<span class="sso-badge">SSO</span>' : ''}</label>
                            <input type="text" id="profile-name" ${isSSO ? 'disabled' : ''} autocomplete="name">
                            ${isSSO ? '<small class="form-hint">Name is managed by your SSO provider</small>' : ''}
                        </div>
                        <div class="form-group">
                            <label for="profile-email">Email</label>
                            <input type="email" id="profile-email" disabled>
                            <small class="form-hint">Email cannot be changed</small>
                        </div>
                        
                        <!-- Slack Integration Section -->
                        <div class="form-group slack-integration">
                            <label>Slack Integration</label>
                            ${this.renderSlackSection()}
                        </div>

                        <!-- Telegram Integration Section -->
                        <div class="form-group telegram-integration">
                            <label>Telegram Integration</label>
                            ${this.renderTelegramSection()}
                        </div>

                        ${this.phone ? `
                        <!-- Phone Section -->
                        <div class="form-group phone-integration">
                            <label>Phone</label>
                            ${this.renderPhoneSection()}
                        </div>` : ''}

                        <div class="form-actions">
                            <button type="submit" class="btn btn-primary" id="save-profile-btn">
                                <i data-lucide="save" class="icon"></i>
                                Save Changes
                            </button>
                        </div>
                    </form>
                </div>

                <!-- API Tokens Section -->
                <div class="profile-section">
                    <h3 class="profile-section-title">
                        <i data-lucide="key" class="section-icon"></i>
                        API Tokens
                    </h3>
                    <p class="section-description">Tokens for programmatic API access. Keep them secret!</p>
                    
                    ${this.newlyCreatedToken ? this.renderNewToken() : ''}
                    
                    <div class="token-list" id="token-list">
                        ${this.renderTokenList()}
                    </div>
                    
                    <form id="token-form" class="token-form">
                        <div class="form-row">
                            <div class="form-group flex-1">
                                <input type="text" id="token-name" placeholder="Token name (e.g., CI/CD)" required autocomplete="off">
                            </div>
                            <div class="form-group">
                                <select id="token-expires">
                                    <option value="">No expiration</option>
                                    <option value="7">7 days</option>
                                    <option value="30" selected>30 days</option>
                                    <option value="90">90 days</option>
                                    <option value="365">1 year</option>
                                </select>
                            </div>
                            <button type="submit" class="btn btn-secondary">
                                <i data-lucide="plus" class="icon"></i>
                                Generate
                            </button>
                        </div>
                    </form>
                </div>
            </div>
        `;

        // Set input values safely
        document.getElementById('profile-name').value = this.currentUser.name || '';
        document.getElementById('profile-email').value = this.currentUser.email || '';

        // Setup dynamic listeners and icons
        this.setupDynamicListeners();
        lucide.createIcons();
    },

    /**
     * Find the Slack external identity for the current user. Identities live in
     * external_identities and are returned on /me as `currentUser.identities`.
     */
    slackIdentity() {
        const list = (this.currentUser && this.currentUser.identities) || [];
        return list.find(id => id.provider === 'slack') || null;
    },

    /**
     * Render Slack integration section based on state
     */
    renderSlackSection() {
        const identity = this.slackIdentity();

        // 1. Connected State
        if (identity) {
            return `
                <div class="slack-connected-state">
                    <div class="slack-info">
                        <i data-lucide="check-circle" class="success-icon"></i>
                        <span>Connected as <strong>${this.escapeHtml(identity.external_id)}</strong></span>
                    </div>
                    <button type="button" class="btn btn-danger btn-sm" id="slack-unbind-btn">
                        <i data-lucide="unlink" class="icon"></i>
                        Disconnect
                    </button>
                </div>
            `;
        }

        // 2. OTP Sent State
        if (this.otpSent) {
            return `
                <div class="slack-otp-state">
                    <div class="slack-input-group">
                        <input type="text" id="slack-user-id" value="${this.escapeAttr(this._tempSlackId || '')}" disabled>
                    </div>
                    <div class="otp-input-group">
                        <input type="text" id="slack-otp-code" placeholder="Enter 6-digit code" maxlength="6" autocomplete="off">
                    </div>
                    <div class="slack-actions">
                        <button type="button" class="btn btn-success btn-sm" id="slack-confirm-otp-btn">
                            Confirm
                        </button>
                        <button type="button" class="btn btn-secondary btn-sm" id="slack-cancel-otp-btn">
                            Cancel
                        </button>
                    </div>
                    <small class="form-hint">Check your Slack Direct Messages for the code.</small>
                </div>
            `;
        }

        // 3. Initial Disconnected State
        return `
            <div class="slack-connect-state">
                <div class="slack-input-group">
                    <input type="text" id="slack-user-id" placeholder="Slack User ID (e.g. U123456)" autocomplete="off">
                    <button type="button" class="btn btn-primary btn-sm" id="slack-request-otp-btn">
                        Connect
                    </button>
                </div>
                <small class="form-hint">Enter your Slack User ID to receive a verification code.</small>
            </div>
        `;
    },

    /**
     * Handle Request OTP
     */
    async handleRequestOTP() {
        const input = document.getElementById('slack-user-id');
        const slackId = input.value.trim();

        if (!slackId) {
            window.showToast && window.showToast('Please enter your Slack User ID', 'error');
            return;
        }

        const btn = document.getElementById('slack-request-otp-btn');
        const originalText = btn.textContent;
        btn.textContent = 'Sending...';
        btn.disabled = true;

        try {
            await API.auth.slack.requestCode(slackId);
            this.otpSent = true;
            this._tempSlackId = slackId;
            this.renderModal(); // Re-render to show OTP input
            window.showToast && window.showToast('OTP sent to Slack', 'success');
        } catch (error) {
            window.showToast && window.showToast(error.message, 'error');
            btn.textContent = originalText;
            btn.disabled = false;
        }
    },

    /**
     * Handle Confirm OTP
     */
    async handleConfirmOTP() {
        const input = document.getElementById('slack-otp-code');
        const code = input.value.trim();

        if (!code) {
            window.showToast && window.showToast('Please enter the verification code', 'error');
            return;
        }

        const btn = document.getElementById('slack-confirm-otp-btn');
        btn.textContent = 'Verifying...';
        btn.disabled = true;

        try {
            const updatedUser = await API.auth.slack.confirmCode(code);
            this.currentUser = updatedUser;
            this.otpSent = false;
            this._tempSlackId = null;
            this.renderModal(); // Re-render to show connected state
            window.showToast && window.showToast('Slack account connected!', 'success');
        } catch (error) {
            window.showToast && window.showToast(error.message, 'error');
            btn.textContent = 'Confirm';
            btn.disabled = false;
        }
    },

    /**
     * Handle Cancel OTP
     */
    handleCancelOTP() {
        this.otpSent = false;
        this._tempSlackId = null;
        this.renderModal();
    },

    /**
     * Handle Unbind Slack
     */
    async handleUnbindSlack() {
        if (!confirm('Are you sure you want to disconnect your Slack account?')) {
            return;
        }

        const btn = document.getElementById('slack-unbind-btn');
        btn.disabled = true;

        try {
            await API.auth.slack.unbind();

            // Manually clear local state - drop the slack identity from the array.
            if (Array.isArray(this.currentUser.identities)) {
                this.currentUser.identities = this.currentUser.identities.filter(i => i.provider !== 'slack');
            }

            this.renderModal(); // Re-render to show disconnected state
            window.showToast && window.showToast('Slack account disconnected', 'success');
        } catch (error) {
            window.showToast && window.showToast(error.message, 'error');
            btn.disabled = false;
        }
    },

    /**
     * Find the linked Telegram identity, if any.
     */
    telegramIdentity() {
        const list = (this.currentUser && this.currentUser.identities) || [];
        return list.find(id => id.provider === 'telegram') || null;
    },

    /**
     * Render the Telegram section. Unlike Slack (OTP), linking is a deep link:
     * the user opens t.me/<bot>?start=<token> and presses Start. That completes
     * server-side asynchronously, so after issuing the link we render an explicit
     * anchor (a real user-gesture click - never window.open, which gets blocked)
     * plus a Refresh action to re-fetch /me.
     */
    renderTelegramSection() {
        const identity = this.telegramIdentity();

        // 1. Connected
        if (identity) {
            const label = identity.display_name || identity.external_id;
            return `
                <div class="slack-connected-state">
                    <div class="slack-info">
                        <i data-lucide="check-circle" class="success-icon"></i>
                        <span>Connected as <strong>${this.escapeHtml(label)}</strong></span>
                    </div>
                    <button type="button" class="btn btn-danger btn-sm" id="telegram-unbind-btn">
                        <i data-lucide="unlink" class="icon"></i>
                        Disconnect
                    </button>
                </div>
            `;
        }

        // 2. Link issued - show the deep link + refresh affordance (buttons right-aligned, like Slack)
        if (this._tgLink) {
            return `
                <div class="slack-otp-state">
                    <div class="slack-actions">
                        <a class="btn btn-primary btn-sm" href="${this.escapeAttr(this._tgLink)}" target="_blank" rel="noopener">
                            <i data-lucide="send" class="icon"></i>
                            Open Telegram
                        </a>
                        <button type="button" class="btn btn-secondary btn-sm" id="telegram-refresh-btn">
                            Refresh
                        </button>
                    </div>
                    <small class="form-hint">Open the link, press Start in Telegram, then click Refresh.</small>
                </div>
            `;
        }

        // 3. Initial disconnected - mirror Slack's [input][Connect] row exactly so the
        // two integration cards line up pixel-for-pixel. There's no real input to type
        // into (linking is a deep link, not an ID), so the left field is a disabled,
        // read-only placeholder; the hint below carries the actual description.
        return `
            <div class="slack-connect-state">
                <div class="slack-input-group">
                    <input type="text" placeholder="Click Connect to generate a link" disabled>
                    <button type="button" class="btn btn-primary btn-sm" id="telegram-connect-btn">
                        Connect
                    </button>
                </div>
                <small class="form-hint">Generates a one-time link to connect your Telegram account.</small>
            </div>
        `;
    },

    /**
     * Handle Connect Telegram - issue a deep link and re-render to show it.
     */
    async handleRequestTelegramLink() {
        const btn = document.getElementById('telegram-connect-btn');
        if (btn) {
            btn.textContent = 'Generating...';
            btn.disabled = true;
        }
        try {
            const resp = await API.auth.telegram.link();
            this._tgLink = resp.link;
            this.renderModal();
        } catch (error) {
            window.showToast && window.showToast(error.message, 'error');
            this.renderModal(); // restore the disconnected state (Connect button with icon)
        }
    },

    /**
     * Handle Refresh - re-fetch /me to pick up the (async) Telegram link.
     */
    async handleRefreshTelegram() {
        try {
            this.currentUser = await API.auth.me();
            this._tgLink = null;
            this.renderModal();
            if (this.telegramIdentity()) {
                window.showToast && window.showToast('Telegram account connected!', 'success');
            } else {
                window.showToast && window.showToast('Not linked yet - open the link and press Start.', 'info');
            }
        } catch (error) {
            window.showToast && window.showToast(error.message, 'error');
        }
    },

    /**
     * Handle Unbind Telegram
     */
    async handleUnbindTelegram() {
        if (!confirm('Are you sure you want to disconnect your Telegram account?')) {
            return;
        }
        const btn = document.getElementById('telegram-unbind-btn');
        if (btn) btn.disabled = true;
        try {
            await API.auth.telegram.unbind();
            if (Array.isArray(this.currentUser.identities)) {
                this.currentUser.identities = this.currentUser.identities.filter(i => i.provider !== 'telegram');
            }
            this._tgLink = null;
            this.renderModal();
            window.showToast && window.showToast('Telegram account disconnected', 'success');
        } catch (error) {
            window.showToast && window.showToast(error.message, 'error');
            if (btn) btn.disabled = false;
        }
    },

    /**
     * Render the phone section: the number, the call with a code that
     * verifies it, the provider it is called through, and the test calls that
     * show a call from each sender number gets through Do Not Disturb.
     */
    renderPhoneSection() {
        const contact = this.phone.contact;

        if (!contact || this.phoneEditing) {
            return `
                <div class="slack-connect-state">
                    <div class="slack-input-group">
                        <input type="tel" id="phone-value" placeholder="+14155550100"
                            value="${this.escapeAttr(contact ? contact.value : '')}" autocomplete="tel">
                        <button type="button" class="btn btn-primary btn-sm" id="phone-save-btn">Save</button>
                        ${contact ? '<button type="button" class="btn btn-secondary btn-sm" id="phone-cancel-edit-btn">Cancel</button>' : ''}
                    </div>
                    <small class="form-hint">International form: a plus, the country code and the number. A new number has to be verified again.</small>
                </div>
            `;
        }

        const verified = !!contact.verified_at;
        const header = `
            <div class="slack-connected-state">
                <div class="slack-info">
                    <i data-lucide="${verified ? 'check-circle' : 'phone'}" class="${verified ? 'success-icon' : ''}"></i>
                    <span><strong>${this.escapeHtml(contact.value)}</strong> ${verified ? 'verified' : 'not verified'}</span>
                </div>
                <div class="slack-actions">
                    <button type="button" class="btn btn-secondary btn-sm" id="phone-edit-btn">Change</button>
                    <button type="button" class="btn btn-danger btn-sm" id="phone-remove-btn">Remove</button>
                </div>
            </div>
        `;

        if (!this.phone.covered) {
            return header + '<small class="form-hint">No phone provider can call this number. Ask an administrator which countries are covered.</small>';
        }

        // The provider choice comes before verification too: if the code call
        // does not get through one provider, the person tries another.
        const providers = this.phone.providers || [];
        const pin = providers.length > 1 ? `
            <div class="form-group">
                <small class="form-hint">Call me through</small>
                <select id="phone-pin-select">
                    <option value="">Default order</option>
                    ${providers.map(p => `<option value="${this.escapeAttr(p.integration_id)}"
                        ${p.integration_id === contact.pinned_integration_id ? 'selected' : ''}>${this.escapeHtml(p.integration)} (${this.escapeHtml(p.number)})</option>`).join('')}
                </select>
                ${contact.pinned_integration_id && !this.phone.pin_active ? '<small class="form-hint">Your choice cannot call this number now; the default order is used.</small>' : ''}
            </div>` : '';

        if (!verified) {
            // The code field shows whenever the number is unverified, not only
            // after this page asked for a call: a call whose outcome was
            // unknown may still arrive, and a call asked for before the
            // profile was reopened is still ringing.
            return header + pin + `
                <div class="slack-otp-state">
                    <div class="slack-actions">
                        <button type="button" class="btn btn-primary btn-sm" id="phone-call-code-btn">Call me with a code</button>
                    </div>
                    <div class="otp-input-group">
                        <input type="text" id="phone-code" placeholder="Enter 6-digit code" maxlength="6" autocomplete="one-time-code">
                        <button type="button" class="btn btn-success btn-sm" id="phone-confirm-code-btn">Confirm</button>
                    </div>
                    <small class="form-hint">We call the number and say a six-digit code. Calls are only placed to a verified number.</small>
                </div>
            `;
        }

        const senders = (this.phone.senders || []).map(sender => `
            <div class="slack-connected-state">
                <div class="slack-info">
                    <i data-lucide="${sender.checked_at ? 'bell-ring' : 'bell-off'}" class="${sender.checked_at ? 'success-icon' : ''}"></i>
                    <span>${this.escapeHtml(sender.number)} ${sender.checked_at ? 'rings through Do Not Disturb' : 'not checked'}</span>
                </div>
                <div class="slack-actions">
                    <button type="button" class="btn btn-secondary btn-sm" data-phone-test="${this.escapeAttr(sender.number)}">Test call</button>
                    <button type="button" class="btn btn-success btn-sm" data-phone-rang="${this.escapeAttr(sender.number)}">It rang</button>
                </div>
            </div>
        `).join('');

        return header + pin + `
            <small class="form-hint">
                Calls come from the numbers below. Save them as one contact
                (<a href="${API.auth.phone.vcardURL}" download="tokayops.vcf">download the card</a>), then let it through:
                on iPhone turn on Emergency Bypass for the contact's ringtone and allow it in every Focus, Sleep included;
                on Android star the contact and allow starred contacts in Do Not Disturb and Bedtime.
                Then make a test call from each number with Do Not Disturb on and the phone silenced.
            </small>
            ${senders}
        `;
    },

    async phoneAction(btn, action, success) {
        if (btn) btn.disabled = true;
        try {
            const result = await action();
            if (result && result.contact !== undefined) {
                this.phone = result;
            }
            if (success) window.showToast && window.showToast(success, 'success');
            this.renderModal();
        } catch (error) {
            window.showToast && window.showToast(error.message, 'error');
            if (btn) btn.disabled = false;
        }
    },

    handleSavePhone() {
        const value = document.getElementById('phone-value').value.trim();
        this.phoneEditing = false;
        return this.phoneAction(document.getElementById('phone-save-btn'), () => API.auth.phone.set(value), 'Phone number saved');
    },

    async handleRemovePhone() {
        if (!confirm('Remove your phone number? You will not be called until you add and verify one again.')) {
            return;
        }
        return this.phoneAction(document.getElementById('phone-remove-btn'), async () => {
            await API.auth.phone.remove();
            return API.auth.phone.get();
        }, 'Phone number removed');
    },

    handleCallWithCode() {
        return this.phoneAction(document.getElementById('phone-call-code-btn'), async () => {
            await API.auth.phone.callWithCode();
        }, 'Calling you now');
    },

    handleConfirmPhoneCode() {
        const code = document.getElementById('phone-code').value.trim();
        return this.phoneAction(document.getElementById('phone-confirm-code-btn'), async () => {
            return API.auth.phone.confirm(code);
        }, 'Phone number verified');
    },

    handlePinPhone(integrationId) {
        return this.phoneAction(null, () => API.auth.phone.pin(integrationId), 'Saved');
    },

    handlePhoneTestCall(sender, btn) {
        return this.phoneAction(btn, () => API.auth.phone.testCall(sender), 'Test call on its way from ' + sender);
    },

    handlePhoneRang(sender, btn) {
        return this.phoneAction(btn, () => API.auth.phone.confirmTestCall(sender), 'Marked');
    },

    /**
     * Render newly created token alert
     */
    renderNewToken() {
        return `
            <div class="token-created-alert">
                <div class="alert-header">
                    <i data-lucide="alert-triangle" class="alert-icon"></i>
                    <strong>Token created! Copy it now - it won't be shown again.</strong>
                </div>
                <div class="token-value">
                    <code id="new-token-value">${this.newlyCreatedToken}</code>
                    <button type="button" class="btn btn-icon" onclick="ProfileModule.copyToken()" title="Copy to clipboard">
                        <i data-lucide="copy"></i>
                    </button>
                </div>
            </div>
        `;
    },

    /**
     * Render token list
     */
    renderTokenList() {
        if (!this.tokens || this.tokens.length === 0) {
            return '<div class="token-empty">No tokens yet</div>';
        }

        return this.tokens.map(token => `
            <div class="token-item">
                <div class="token-info">
                    <span class="token-name">${this.escapeHtml(token.name)}</span>
                    <span class="token-meta">
                        Created ${this.formatDate(token.created_at)}
                        ${token.last_used_at ? ` • Last used ${this.formatDate(token.last_used_at)}` : ' • Never used'}
                        ${token.expires_at ? ` • Expires ${this.formatDate(token.expires_at)}` : ''}
                    </span>
                </div>
                <button type="button" class="btn btn-danger btn-sm" onclick="ProfileModule.revokeToken('${token.id}')">
                    <i data-lucide="trash-2" class="icon"></i>
                    Revoke
                </button>
            </div>
        `).join('');
    },

    /**
     * Handle profile form submit
     */
    async handleProfileSubmit(e) {
        e.preventDefault();

        const data = {};
        const isSSO = this.currentUser.auth_provider === 'oidc';

        if (!isSSO) {
            const name = document.getElementById('profile-name').value.trim();
            if (name !== this.currentUser.name) {
                data.name = name;
            }
        }

        // Note: Slack ID is now handled separately via bind/unbind buttons

        if (Object.keys(data).length === 0) {
            window.showToast && window.showToast('No changes to save', 'info');
            return;
        }

        try {
            const updated = await API.auth.updateMe(data);
            this.currentUser = updated;
            window.showToast && window.showToast('Profile updated', 'success');

            // Update header user name if present
            const userName = document.getElementById('dropdown-user-name');
            if (userName) userName.textContent = updated.name;
        } catch (error) {
            window.showToast && window.showToast(error.message, 'error');
        }
    },

    /**
     * Handle token creation
     */
    async handleTokenCreate(e) {
        e.preventDefault();

        const name = document.getElementById('token-name').value.trim();
        const expiresSelect = document.getElementById('token-expires');
        const expiresIn = expiresSelect.value ? parseInt(expiresSelect.value) : null;

        if (!name) {
            window.showToast && window.showToast('Token name is required', 'error');
            return;
        }

        try {
            const result = await API.tokens.create(name, expiresIn);
            this.newlyCreatedToken = result.token;

            // Refresh token list
            const tokensResp = await API.tokens.list();
            this.tokens = tokensResp.tokens || [];

            // Re-render
            this.renderModal();

            window.showToast && window.showToast('Token created', 'success');
        } catch (error) {
            window.showToast && window.showToast(error.message, 'error');
        }
    },

    /**
     * Copy new token to clipboard
     */
    async copyToken() {
        const tokenValue = document.getElementById('new-token-value');
        if (!tokenValue) return;

        try {
            await navigator.clipboard.writeText(tokenValue.textContent);
            window.showToast && window.showToast('Token copied to clipboard', 'success');
        } catch (error) {
            // Fallback for older browsers
            const range = document.createRange();
            range.selectNode(tokenValue);
            window.getSelection().removeAllRanges();
            window.getSelection().addRange(range);
            document.execCommand('copy');
            window.getSelection().removeAllRanges();
            window.showToast && window.showToast('Token copied', 'success');
        }
    },

    /**
     * Revoke a token
     */
    async revokeToken(tokenId) {
        if (!confirm('Are you sure you want to revoke this token? It will stop working immediately.')) {
            return;
        }

        try {
            await API.tokens.delete(tokenId);

            // Refresh token list
            const tokensResp = await API.tokens.list();
            this.tokens = tokensResp.tokens || [];

            // Re-render token list only
            const list = document.getElementById('token-list');
            if (list) {
                list.innerHTML = this.renderTokenList();
                lucide.createIcons();
            }

            window.showToast && window.showToast('Token revoked', 'success');
        } catch (error) {
            window.showToast && window.showToast(error.message, 'error');
        }
    },

    /**
     * Format date for display
     */
    formatDate(dateStr) {
        if (!dateStr) return '';
        const date = new Date(dateStr);
        const now = new Date();
        const diff = now - date;

        // Less than 24 hours
        if (diff < 86400000) {
            const hours = Math.floor(diff / 3600000);
            if (hours < 1) {
                const mins = Math.floor(diff / 60000);
                return mins < 1 ? 'just now' : `${mins}m ago`;
            }
            return `${hours}h ago`;
        }

        // Less than 7 days
        if (diff < 604800000) {
            const days = Math.floor(diff / 86400000);
            return `${days}d ago`;
        }

        return date.toLocaleDateString();
    },

    /**
     * Escape HTML (for content)
     */
    escapeHtml(text) {
        if (!text) return '';
        const div = document.createElement('div');
        div.textContent = text;
        return div.innerHTML;
    },

    /**
     * Escape for HTML attributes (escapes quotes)
     */
    escapeAttr(text) {
        if (!text) return '';
        return text
            .replace(/&/g, '&amp;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;');
    }
};

// Export
window.ProfileModule = ProfileModule;
