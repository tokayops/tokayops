/**
 * One open of the shared modal, as something that can be ended.
 *
 * The modal shell is a singleton: one overlay, one title, one body, one
 * footer, reused by every editor in the app. What is not a singleton is the
 * work an open modal has outstanding - a request on its way, a listener on
 * `document`, a form waiting to be filled in. Tying that work to the open
 * rather than to the module is what stops one modal answering for another.
 *
 * Three things follow, and each of them was a bug before it was a rule:
 *
 *  - `close()` is idempotent. There are four ways out of a modal and they run
 *    into each other; the second one to arrive has to be a no-op rather than a
 *    second teardown.
 *  - Listeners are registered with `session.signal`, so they belong to one
 *    open and to nobody else. The delegated dispatcher that opens these modals
 *    is not part of any session and outlives all of them; taking it down with
 *    a modal would leave the page with dead buttons.
 *  - Work that resumes after an `await` asks `session.closed` before touching
 *    the DOM. Dropping the listeners does not stop a request already in
 *    flight, and that is the sequence that let a closed modal rewrite the body
 *    of the one that replaced it.
 *
 * Opening a session ends the one before it, because opening a modal is exactly
 * the moment the previous one stopped being on screen.
 */

import { closeModalById } from '/js/core/utils.js';

const OVERLAY_ID = 'modal-overlay';

/** The parts of the shared shell, looked up in one place. */
export function modalShell() {
    return {
        overlay: document.getElementById(OVERLAY_ID),
        title: document.getElementById('modal-title'),
        body: document.getElementById('modal-body'),
        footer: document.getElementById('modal-footer'),
    };
}

/** The session currently on screen, or null. Never more than one. */
let active = null;

/**
 * Begin a session for a modal that is being opened.
 *
 * @param {Object} [handlers]
 * @param {(event: Event) => boolean} [handlers.onDismiss] - the shell was
 *        dismissed by the X, by the background or by Escape. The event is
 *        passed because those three do not always mean the same thing: with a
 *        menu open, Escape belongs to the menu. Return true to take over what
 *        happens next: the dismissal is stopped where it is, and the session
 *        stays open until its owner closes it. Anything else lets the app
 *        close the modal, and this session ends.
 * @returns {{signal: AbortSignal, closed: boolean, close: () => void, closeModal: () => void}}
 */
export function beginModalSession({ onDismiss } = {}) {
    // Before anything else: whatever was on screen is not any more.
    active?.close();

    const controller = new AbortController();

    const session = {
        signal: controller.signal,
        // The same fact the listeners were removed by, rather than a flag kept
        // alongside it: code that resumes after an `await` asks this, and code
        // that made a request passed the signal, and the two must not be able
        // to disagree about whether this modal is still there.
        get closed() {
            return controller.signal.aborted;
        },
        close() {
            if (controller.signal.aborted) return;
            if (active === session) active = null;
            controller.abort();
        },
        closeModal() {
            session.close();
            closeModalById(OVERLAY_ID);
        },
    };

    // Capture phase, so this runs before the app's own close handlers whatever
    // order the listeners were registered in. A session is created while the
    // page is already running, which puts it after everything bound at start-up
    // - and being second is the difference between redirecting a dismissal and
    // watching the modal close underneath you.
    const dismissal = (event) => {
        if (onDismiss && onDismiss(event) === true) {
            event.preventDefault();
            event.stopImmediatePropagation();
            return;
        }
        session.close();
    };

    document.addEventListener('click', (event) => {
        const byClose = event.target.closest?.('#modal-close');
        const byBackground = event.target.id === OVERLAY_ID;
        if (byClose || byBackground) dismissal(event);
    }, { capture: true, signal: controller.signal });

    document.addEventListener('keydown', (event) => {
        if (event.key === 'Escape') dismissal(event);
    }, { capture: true, signal: controller.signal });

    active = session;
    return session;
}
