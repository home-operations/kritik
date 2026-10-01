import type { Attachment } from 'svelte/attachments';

// focusOnMount focuses an overlay's first field, or the overlay itself, as
// soon as it is in the DOM.
export const focusOnMount: Attachment<HTMLElement> = (node) => {
  node.focus();
};

// revealInNav scrolls a navigation's current item into view, for a strip
// that scrolls sideways on a narrow screen.
export const revealInNav: Attachment<HTMLElement> = (node) => {
  node.scrollIntoView({ block: 'nearest', inline: 'nearest' });
};

// focusWhenShown focuses the element target selects once the page shows
// it, since a page loads its data first: the field inside it, as a search
// lands on a setting, or with section the element itself, scrolled to the
// top, as a jump to a part of a page does. A choice drawn as a segmented
// control focuses its current option.
export function focusWhenShown(target: string, section = false): void {
  const deadline = performance.now() + 3000;
  const attempt = (): void => {
    const el = document.querySelector<HTMLElement>(target);
    if (!el) {
      if (performance.now() < deadline) requestAnimationFrame(attempt);
      return;
    }
    const field = section
      ? null
      : el.matches('input, select, textarea')
        ? el
        : el.querySelector<HTMLElement>('input, select, textarea, [role="radio"][aria-checked="true"]');
    const focus = field ?? el;
    if (!field && !focus.hasAttribute('tabindex')) focus.setAttribute('tabindex', '-1');
    focus.scrollIntoView({ block: section ? 'start' : 'center' });
    focus.focus({ preventScroll: true });
  };
  requestAnimationFrame(attempt);
}
