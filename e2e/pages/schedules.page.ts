import { Page, Locator, expect } from '@playwright/test';
import { waitForAppReady } from './app.utils';

export class SchedulesPage {
  readonly page: Page;

  // On-call widget
  readonly oncallWidget: Locator;
  readonly editScheduleBtn: Locator;
  readonly viewScheduleBtn: Locator;
  readonly createOverrideBtn: Locator;

  // Schedule configuration modal
  readonly scheduleModal: Locator;
  readonly scheduleModalClose: Locator;
  readonly scheduleForm: Locator;
  readonly timezoneSelect: Locator;
  readonly l1RotationType: Locator;
  readonly l1HandoffTime: Locator;
  readonly l1HandoffDay: Locator;
  readonly l2Enabled: Locator;
  readonly l2EscalationTimeout: Locator;
  readonly l2RotationType: Locator;
  readonly l2HandoffTime: Locator;
  readonly l2HandoffDay: Locator;

  // L1 groups editor (multi-oncall)
  readonly l1GroupsEditor: Locator;
  readonly l1AddGroupBtn: Locator;

  // L2 user lists (still flat)
  readonly l2AvailableList: Locator;
  readonly l2UsersList: Locator;

  // Override modal
  readonly overrideModal: Locator;
  readonly overrideUserSelect: Locator;
  readonly overrideStartDate: Locator;
  readonly overrideStartTime: Locator;
  readonly overrideEndDate: Locator;
  readonly overrideEndTime: Locator;
  readonly overrideReason: Locator;
  readonly overrideSubmitBtn: Locator;
  readonly overrideCancelBtn: Locator;

  // Overrides list
  readonly overridesList: Locator;
  readonly overrideItems: Locator;

  // Calendar view
  readonly calendarList: Locator;
  readonly calendarOverrideEntries: Locator;

  // Context menu
  readonly contextMenu: Locator;
  readonly contextMenuEdit: Locator;
  readonly contextMenuDelete: Locator;

  // Save button
  readonly saveScheduleBtn: Locator;

  // Delete button
  readonly deleteScheduleBtn: Locator;

  // Toast
  readonly toastContainer: Locator;

  constructor(page: Page) {
    this.page = page;

    // On-call widget
    this.oncallWidget = page.locator('.on-call-widget, .oncall-row');
    this.editScheduleBtn = page.locator('.edit-schedule-btn');
    this.viewScheduleBtn = page.locator('.view-schedule-btn');
    this.createOverrideBtn = page.locator('.create-override-btn');

    // Schedule configuration modal (uses generic modal-overlay)
    this.scheduleModal = page.locator('#modal-overlay');
    this.scheduleModalClose = page.locator('#modal-close');
    this.scheduleForm = page.locator('#schedule-form');
    this.timezoneSelect = page.locator('#schedule-timezone');
    this.l1RotationType = page.locator('#l1-rotation-type');
    this.l1HandoffTime = page.locator('#l1-handoff-time');
    this.l1HandoffDay = page.locator('#l1-handoff-day');
    this.l2Enabled = page.locator('#l2-enabled');
    this.l2EscalationTimeout = page.locator('#l2-escalation-timeout');
    this.l2RotationType = page.locator('#l2-rotation-type');
    this.l2HandoffTime = page.locator('#l2-handoff-time');
    this.l2HandoffDay = page.locator('#l2-handoff-day');

    // L1 groups editor
    this.l1GroupsEditor = page.locator('#l1-groups-editor');
    this.l1AddGroupBtn = page.locator('#l1-add-group');

    // L2 user lists (still flat sortable)
    this.l2AvailableList = page.locator('#l2-available');
    this.l2UsersList = page.locator('#l2-users-list');

    // Override modal (also uses generic modal-overlay)
    this.overrideModal = page.locator('#modal-overlay');
    this.overrideUserSelect = page.locator('#override-user');
    this.overrideStartDate = page.locator('#override-start'); // datetime-local
    this.overrideStartTime = page.locator('#override-start'); // same field (datetime-local)
    this.overrideEndDate = page.locator('#override-end'); // datetime-local
    this.overrideEndTime = page.locator('#override-end'); // same field (datetime-local)
    this.overrideReason = page.locator('#override-reason');
    // Submit button is in #modal-footer, linked via form="override-form" attribute
    this.overrideSubmitBtn = page.locator('#modal-footer button[type="submit"]');
    this.overrideCancelBtn = page.locator('#override-cancel');

    // Overrides list
    this.overridesList = page.locator('.overrides-list');
    this.overrideItems = page.locator('.override-item');

    // Calendar view (monthlyScheduleCalendar renders .calendar-list)
    this.calendarList = page.locator('.calendar-list');
    this.calendarOverrideEntries = page.locator('.calendar-entry.layer-override');

    // Context menu (appended to body, outside modal)
    this.contextMenu = page.locator('.override-context-menu');
    this.contextMenuEdit = page.locator('.override-context-menu-item[data-action="edit"]');
    this.contextMenuDelete = page.locator('.override-context-menu-item[data-action="delete"]');

    // Save button (schedule form submit)
    this.saveScheduleBtn = page.locator('#schedule-form button[type="submit"]');

    // Delete button (Danger Zone)
    this.deleteScheduleBtn = page.locator('.delete-schedule-btn');

    // Toast
    this.toastContainer = page.locator('#toast-container');
  }

  async gotoTeams() {
    await this.page.goto('/#/cfg/teams');
    await waitForAppReady(this.page);
  }

  async gotoOnCall() {
    await this.page.goto('/#/ops/oncall');
    await waitForAppReady(this.page);
    await this.page.waitForSelector('.oncall-row, .empty-state', { state: 'visible', timeout: 10000 });
  }

  async openTeamModal(teamId: string) {
    const teamCard = this.page.locator(`.team-card[data-team-id="${teamId}"]`);
    await teamCard.click();
    await this.page.waitForTimeout(500);
  }

  async openScheduleConfig(teamId: string) {
    // Find edit schedule button for specific team
    const editBtn = this.page.locator(`.edit-schedule-btn[data-team-id="${teamId}"]`).first();
    if (await editBtn.isVisible()) {
      await editBtn.click();
    } else {
      // Try within team modal
      await this.editScheduleBtn.first().click();
    }
    await expect(this.scheduleModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async closeScheduleModal() {
    // Use cancel button or modal close
    const cancelBtn = this.page.locator('#schedule-cancel, #modal-close');
    await cancelBtn.first().click();
    await expect(this.scheduleModal).not.toHaveClass(/active/, { timeout: 10000 });
  }

  async setTimezone(timezone: string) {
    await this.timezoneSelect.selectOption(timezone);
  }

  async setL1RotationType(type: 'daily' | 'weekly') {
    await this.l1RotationType.selectOption(type);
  }

  async setL1HandoffTime(time: string) {
    await this.l1HandoffTime.fill(time);
  }

  async setL1HandoffDay(day: number) {
    await this.l1HandoffDay.selectOption(day.toString());
  }

  async enableL2(enabled: boolean) {
    const isChecked = await this.l2Enabled.isChecked();
    if (isChecked !== enabled) {
      await this.l2Enabled.click();
    }
  }

  async setL2EscalationTimeout(minutes: number) {
    await this.l2EscalationTimeout.fill(minutes.toString());
  }

  /**
   * Count total users across all L1 groups (sum of chips in editor).
   */
  async getL1UserCount(): Promise<number> {
    return await this.l1GroupsEditor.locator('.user-chip').count();
  }

  /**
   * Count L1 groups in the editor.
   */
  async getL1GroupCount(): Promise<number> {
    return await this.l1GroupsEditor.locator('.group-row').count();
  }

  async getL2UserCount(): Promise<number> {
    return await this.l2UsersList.locator('.rotation-user').count();
  }

  /**
   * Click "Add Group" to append a new empty group row.
   */
  async addL1Group() {
    await this.l1AddGroupBtn.click();
  }

  /**
   * Add a user to the group at the given index (0-based).
   */
  async addUserToL1Group(groupIndex: number, userId: string) {
    const row = this.l1GroupsEditor.locator('.group-row').nth(groupIndex);
    await row.locator('.group-add-user').selectOption(userId);
  }

  /**
   * Remove a user (by ID) from the group at the given index.
   */
  async removeUserFromL1Group(groupIndex: number, userId: string) {
    const row = this.l1GroupsEditor.locator('.group-row').nth(groupIndex);
    const chip = row.locator(`.user-chip[data-user-id="${userId}"]`);
    await chip.locator('.chip-remove').click();
  }

  /**
   * Delete the group at the given index entirely.
   */
  async deleteL1Group(groupIndex: number) {
    const row = this.l1GroupsEditor.locator('.group-row').nth(groupIndex);
    await row.locator('.group-delete').click();
  }

  async saveSchedule() {
    await this.saveScheduleBtn.click();
  }

  async deleteSchedule() {
    await this.deleteScheduleBtn.click();
  }

  async openCreateOverrideModal(teamId?: string) {
    if (teamId) {
      const btn = this.page.locator(`.create-override-btn[data-team-id="${teamId}"]`).first();
      await btn.click();
    } else {
      await this.createOverrideBtn.first().click();
    }
    await expect(this.overrideModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async closeOverrideModal() {
    await this.overrideCancelBtn.click();
    await expect(this.overrideModal).not.toHaveClass(/active/, { timeout: 10000 });
  }

  /**
   * Cancel override form expecting to return to a parent view (e.g. calendar).
   * The modal stays active — only the content changes back.
   */
  async cancelOverrideAndReturnToCalendar() {
    await this.overrideCancelBtn.click();
    // Modal stays active, but calendar content re-renders
    await expect(this.calendarList).toBeVisible({ timeout: 5000 });
  }

  async createOverride(userId: string, startDate: string, startTime: string, endDate: string, endTime: string, reason?: string) {
    await this.overrideUserSelect.selectOption(userId);
    await this.overrideStartDate.fill(startDate);
    await this.overrideStartTime.fill(startTime);
    await this.overrideEndDate.fill(endDate);
    await this.overrideEndTime.fill(endTime);
    if (reason) {
      await this.overrideReason.fill(reason);
    }
    await this.overrideSubmitBtn.click();
  }

  async getOverrideCount(): Promise<number> {
    return await this.overrideItems.count();
  }

  async deleteOverride(index: number) {
    const item = this.overrideItems.nth(index);
    const deleteBtn = item.locator('.delete-override-btn');
    await deleteBtn.click();
  }

  async expectScheduleModalVisible() {
    await expect(this.scheduleModal).toHaveClass(/active/, { timeout: 10000 });
  }

  async expectScheduleModalHidden() {
    await expect(this.scheduleModal).not.toHaveClass(/active/, { timeout: 10000 });
  }

  async expectOverrideModalVisible() {
    await expect(this.overrideModal).toHaveClass(/active/, { timeout: 10000 });
    // The overlay is already active whenever any modal is open, including the
    // calendar this one is reached from - so waiting on it alone would return
    // before the override modal had rendered.
    await expect(this.page.locator('#override-form')).toBeVisible({ timeout: 10000 });
  }

  async expectOverrideModalHidden() {
    await expect(this.overrideModal).not.toHaveClass(/active/, { timeout: 10000 });
  }

  async expectToastVisible(message?: string) {
    const toast = this.toastContainer.locator('.toast');
    await expect(toast.first()).toBeVisible();
    if (message) {
      await expect(toast.first()).toContainText(message);
    }
  }

  // ========================================
  // Calendar view helpers
  // ========================================

  async openCalendarView(teamId: string) {
    const btn = this.page.locator(`.view-schedule-btn[data-team-id="${teamId}"]`).first();
    await btn.click();
    await expect(this.scheduleModal).toHaveClass(/active/, { timeout: 10000 });
    await expect(this.calendarList).toBeVisible({ timeout: 5000 });
  }

  async closeCalendarView() {
    const closeBtn = this.page.locator('#calendar-close');
    await closeBtn.waitFor({ state: 'attached', timeout: 5000 });
    await closeBtn.waitFor({ state: 'visible', timeout: 5000 });
    await closeBtn.click();
    await expect(this.scheduleModal).not.toHaveClass(/active/, { timeout: 10000 });
  }

  async getCalendarOverrideEntries(): Promise<Locator> {
    return this.calendarOverrideEntries;
  }

  async clickCalendarOverride(index: number = 0) {
    const entry = this.calendarOverrideEntries.nth(index);
    await entry.click();
  }

  async expectContextMenuVisible() {
    await expect(this.contextMenu).toHaveClass(/active/, { timeout: 5000 });
  }

  async expectContextMenuHidden() {
    await expect(this.contextMenu).not.toHaveClass(/active/, { timeout: 10000 });
  }

  async contextMenuClickEdit() {
    await expect(this.contextMenuEdit).toBeVisible({ timeout: 5000 });
    await this.contextMenuEdit.click({ force: true });
  }

  async contextMenuClickDelete() {
    await expect(this.contextMenuDelete).toBeVisible({ timeout: 5000 });
    await this.contextMenuDelete.click({ force: true });
  }

  async editOverrideFromList(index: number) {
    const item = this.overrideItems.nth(index);
    const editBtn = item.locator('.edit-override-btn');
    await editBtn.click();
  }
}
