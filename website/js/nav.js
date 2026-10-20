/* Mobile navigation drawer, shared by every page.
 *
 * The header keeps its links in one place - the same <nav class="nav-links">
 * the desktop bar lays out in a row - and the stylesheet turns that same
 * element into a drawer under 1024px. So there is no second copy of the menu
 * to drift out of date: this file only carries the open/closed state, and the
 * breakpoint that hides the drawer is the same one the CSS uses.
 *
 * State lives in three places at once because three things have to agree: the
 * class that slides the panel, aria-expanded on the button that owns it, and a
 * body class that stops the page scrolling behind an open panel. Escape, the
 * scrim, a tap on a link and a resize past the breakpoint all close it. */
(function () {
  'use strict';

  var burger = document.querySelector('[data-nav-toggle]');
  var panel = document.querySelector('.nav-links');
  var scrim = document.querySelector('.nav-scrim');
  if (!burger || !panel) return;

  var WIDE = '(min-width: 1024px)';
  var open = false;

  function setOpen(next) {
    open = next;
    panel.classList.toggle('on', next);
    if (scrim) scrim.classList.toggle('on', next);
    burger.setAttribute('aria-expanded', next ? 'true' : 'false');
    burger.setAttribute('aria-label', next ? 'Close menu' : 'Open menu');
    document.body.classList.toggle('nav-open', next);
  }

  burger.addEventListener('click', function () { setOpen(!open); });
  if (scrim) scrim.addEventListener('click', function () { setOpen(false); });

  // Any link inside the panel closes it first. Tapping the page you are
  // already on does not reload, so without this the panel would sit open over
  // the page it belongs to.
  panel.addEventListener('click', function (e) {
    if (e.target && e.target.closest && e.target.closest('a')) setOpen(false);
  });

  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && open) {
      setOpen(false);
      burger.focus();
    }
  });

  // Rotating a tablet past the breakpoint hides the panel on its own; the
  // state has to go with it or the bar comes back with the page still locked.
  if (window.matchMedia) {
    var mq = matchMedia(WIDE);
    var onChange = function (e) { if (e.matches && open) setOpen(false); };
    if (mq.addEventListener) mq.addEventListener('change', onChange);
    else if (mq.addListener) mq.addListener(onChange);
  }
})();