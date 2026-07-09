/**
 * TokayOps Users Module
 * User management functionality
 */

import { State } from '/js/core/state.js';
import { Elements, showToast } from '/js/core/utils.js';
import { ViewManager } from '/js/core/viewManager.js';

/**
 * Bind user-related event listeners
 */
export function bindUsersEvents() {
    // Sidebar link
    const userLink = document.querySelector('.nav-section-link[data-view="users"]');
    if (userLink) {
        userLink.addEventListener('click', (e) => {
            e.preventDefault();
            showUsersView();
        });
    }

    // Add User button (sidebar)
    const addUserSidebarBtn = document.getElementById('add-user-sidebar-btn');
    if (addUserSidebarBtn) {
        addUserSidebarBtn.addEventListener('click', () => {
            openUserModal();
        });
    }

    // Add User button (main section)
    if (Elements.addUserBtn) {
        Elements.addUserBtn.addEventListener('click', () => {
            openUserModal();
        });
    }

    // User modal close
    const userModalClose = document.getElementById('user-modal-close');
    if (userModalClose) {
        userModalClose.addEventListener('click', closeUserModal);
    }
}

/**
 * Show the users management view
 */
export async function showUsersView() {
    State.currentView = 'users';
    State.currentTeam = null;

    // Update Sidebar
    document.querySelectorAll('.sidebar-nav .nav-item').forEach(nav => nav.classList.remove('active'));

    // Use ViewManager to show users view
    ViewManager.show('users', { showStats: false, showViewToggle: false });

    loadUsers();
}

/**
 * Load and render users list
 */
export async function loadUsers() {
    if (Elements.usersLoading) Elements.usersLoading.style.display = 'flex';
    Elements.usersGrid.innerHTML = '';

    try {
        const response = await API.users.list();
        State.users = response.users || [];
        Elements.usersGrid.innerHTML = Components.usersList(State.users);

        bindUserCardEvents();

        if (window.lucide) lucide.createIcons();
    } catch (error) {
        showToast('Failed to load users', 'error');
    } finally {
        if (Elements.usersLoading) Elements.usersLoading.style.display = 'none';
    }
}

/**
 * Bind edit/delete buttons on user cards
 */
function bindUserCardEvents() {
    Elements.usersGrid.querySelectorAll('.user-row').forEach(row => {
        row.addEventListener('click', (e) => {
            if (e.target.closest('button')) return;
            const userId = row.dataset.userId;
            const user = State.users.find(u => u.id === userId);
            if (user) openUserModal(user);
        });
    });

}

/**
 * Open user create/edit modal
 */
export function openUserModal(user = null) {
    State.editingUser = user;
    Elements.userModalTitle.textContent = user ? 'Edit User' : 'Create User';
    Elements.userModalBody.innerHTML = Components.userFormModal(user);

    // Render footer
    const isEdit = user !== null;
    Elements.userModalFooter.innerHTML = `
        <button type="button" class="btn btn-secondary" id="user-form-cancel">Cancel</button>
        <button type="submit" form="user-form" class="btn btn-primary" id="user-form-submit">${isEdit ? 'Save Changes' : 'Create User'}</button>
    `;

    Elements.userModalOverlay.classList.add('active');
    document.body.style.overflow = 'hidden';
    if (window.lucide) lucide.createIcons();

    // Bind form submit
    const form = document.getElementById('user-form');
    form.addEventListener('submit', handleUserSubmit);

    const cancelBtn = document.getElementById('user-form-cancel');
    cancelBtn.addEventListener('click', closeUserModal);

    const resetPasswordBtn = document.getElementById('user-reset-password-btn');
    if (resetPasswordBtn) {
        resetPasswordBtn.addEventListener('click', handlePasswordReset);
    }

    const deleteBtn = document.querySelector('.delete-user-modal-btn');
    if (deleteBtn) {
        deleteBtn.addEventListener('click', handleUserDelete);
    }
}

/**
 * Close user modal
 */
export function closeUserModal() {
    Elements.userModalOverlay.classList.remove('active');
    document.body.style.overflow = '';
    State.editingUser = null;
}

/**
 * Handle user form submission
 */
async function handleUserSubmit(e) {
    e.preventDefault();
    const formData = new FormData(e.target);
    const data = Object.fromEntries(formData.entries());

    try {
        if (State.editingUser) {
            const payload = {
                name: data.name,
                email: data.email,
            };
            if (Permissions.isAdmin()) {
                if (data.role) {
                    payload.role = data.role;
                }
                // Linking external accounts (Slack, …) is via the per-provider link flow
                // (POST /me/slack/request-code, …), not admin user-edit.
            }

            await API.users.update(State.editingUser.id, payload);
            showToast('User updated', 'success');
        } else {
            if (!data.password) delete data.password;
            if (!Permissions.isAdmin()) delete data.role;
            await API.users.create(data);
            showToast('User created', 'success');
        }
        closeUserModal();
        loadUsers();
    } catch (error) {
        showToast(error.message, 'error');
    }
}

async function handlePasswordReset() {
    if (!State.editingUser) return;
    const passwordInput = document.getElementById('user-password-reset');
    const newPassword = passwordInput?.value?.trim() || '';
    if (!newPassword) {
        showToast('Enter a new password', 'error');
        return;
    }
    const confirmMsg = `Reset password for ${State.editingUser.email || State.editingUser.name}?`;
    if (!confirm(confirmMsg)) return;

    try {
        await API.users.updatePassword(State.editingUser.id, newPassword);
        passwordInput.value = '';
        showToast('Password reset', 'success');
    } catch (error) {
        showToast(error.message, 'error');
    }
}

async function handleUserDelete() {
    if (!State.editingUser) return;
    const name = State.editingUser.email || State.editingUser.name || State.editingUser.id;
    if (!confirm(`Delete user ${name}? This cannot be undone.`)) return;

    try {
        await API.users.delete(State.editingUser.id);
        showToast('User deleted', 'success');
        closeUserModal();
        loadUsers();
    } catch (error) {
        showToast(error.message, 'error');
    }
}
