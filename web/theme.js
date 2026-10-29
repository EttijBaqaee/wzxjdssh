(() => {
  'use strict';
  const key = 'wzxjdssh-theme';
  const browserTheme = matchMedia('(prefers-color-scheme: dark)');
  let chosen = null;
  try { const saved = localStorage.getItem(key); if (saved === 'light' || saved === 'dark') chosen = saved; } catch {}
  function apply() {
    const dark = (chosen || (browserTheme.matches ? 'dark' : 'light')) === 'dark';
    document.documentElement.dataset.theme = dark ? 'dark' : 'light';
    document.querySelectorAll('[data-theme-toggle]').forEach(button => {
      button.textContent = dark ? '☀ 明亮模式' : '☾ 暗黑模式';
      button.setAttribute('aria-label', dark ? '切换到明亮模式' : '切换到暗黑模式');
      button.setAttribute('aria-pressed', String(dark));
    });
  }
  apply();
  browserTheme.addEventListener('change', () => { if (!chosen) apply(); });
  addEventListener('storage', event => {
    if (event.key === key) { chosen = ['light', 'dark'].includes(event.newValue) ? event.newValue : null; apply(); }
  });
  document.addEventListener('DOMContentLoaded', () => {
    document.querySelectorAll('[data-theme-toggle]').forEach(button => button.addEventListener('click', () => {
      chosen = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';
      try { localStorage.setItem(key, chosen); } catch {}
      apply();
    }));
    apply();
  });
})();
