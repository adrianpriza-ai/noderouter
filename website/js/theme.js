/* Theme controller shared by every page.
 *
 * The page head still runs a tiny inline script that sets data-theme before
 * first paint (so there is no flash of the wrong theme); this file then takes
 * over: it re-applies the saved choice, wires the sun/moon/system buttons and
 * follows the OS when the choice is "auto". The choice is remembered under
 * noderouter:theme and is shared across pages. */
(function () {
  'use strict';

  var KEY = 'noderouter:theme';

  function prefersDark() {
    return !!(window.matchMedia && matchMedia('(prefers-color-scheme: dark)').matches);
  }
  function systemTheme() { return prefersDark() ? 'dark' : 'light'; }
  function saved() {
    try { return localStorage.getItem(KEY) || 'auto'; } catch (e) { return 'auto'; }
  }

  function apply(theme) {
    var root = document.documentElement;
    root.dataset.theme = (theme === 'auto') ? systemTheme() : theme;
    root.dataset.themeChoice = theme;
    var buttons = document.querySelectorAll('[data-theme-btn]');
    for (var i = 0; i < buttons.length; i++) {
      var on = buttons[i].getAttribute('data-theme-btn') === theme;
      buttons[i].classList.toggle('on', on);
      buttons[i].setAttribute('aria-pressed', on ? 'true' : 'false');
    }
  }

  function init() {
    apply(saved());

    var buttons = document.querySelectorAll('[data-theme-btn]');
    for (var i = 0; i < buttons.length; i++) {
      buttons[i].addEventListener('click', function () {
        var next = this.getAttribute('data-theme-btn');
        try { localStorage.setItem(KEY, next); } catch (e) {}
        apply(next);
      });
    }

    if (window.matchMedia) {
      var mq = matchMedia('(prefers-color-scheme: dark)');
      var onChange = function () { if (saved() === 'auto') apply('auto'); };
      if (mq.addEventListener) mq.addEventListener('change', onChange);
      else if (mq.addListener) mq.addListener(onChange);
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
