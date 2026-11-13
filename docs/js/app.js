/* ===================================================
   docker agent docs – site utilities
   (theme, search, TOC, copy buttons)
   =================================================== */

// ---------- DOM references ----------
const $content      = document.getElementById('page-content');
const $searchInput  = document.getElementById('search-input');
const $searchOverlay = document.getElementById('search-overlay');
const $searchModal   = document.getElementById('search-modal-input');
const $searchResults = document.getElementById('search-results');

// ---------- Theme ----------
function updateThemeToggleState() {
  const isLight = document.documentElement.getAttribute('data-theme') === 'light';
  const toggle = document.getElementById('theme-toggle');
  if (!toggle) return;
  toggle.setAttribute('aria-pressed', String(isLight));
  toggle.setAttribute('aria-label', isLight ? 'Switch to dark theme' : 'Switch to light theme');
}

function initTheme() {
  // ?theme=light|dark deterministically forces a theme, bypassing both
  // localStorage and the OS preference — used by the pa11y-ci both-theme
  // gate so CI doesn't depend on the headless browser's default color
  // scheme (see docs/.pa11yci.json).
  const forced = new URLSearchParams(location.search).get('theme');
  const saved = forced === 'light' || forced === 'dark' ? forced : localStorage.getItem('docker-agent-docs-theme');
  if (saved === 'light') {
    document.documentElement.setAttribute('data-theme', 'light');
  } else if (!saved && window.matchMedia?.('(prefers-color-scheme: light)').matches) {
    document.documentElement.setAttribute('data-theme', 'light');
  }
  // Dark is the default — no attribute needed (CSS :root is dark)
  updateThemeToggleState();
}

function toggleTheme() {
  const isLight = document.documentElement.getAttribute('data-theme') === 'light';
  if (isLight) {
    document.documentElement.removeAttribute('data-theme');
    localStorage.setItem('docker-agent-docs-theme', 'dark');
  } else {
    document.documentElement.setAttribute('data-theme', 'light');
    localStorage.setItem('docker-agent-docs-theme', 'light');
  }
  updateThemeToggleState();
}

// ---------- Table of Contents ----------
// The right-column aside on each page contains:
//   1. An "On this page" heading + nested links to in-page
//      <h2 id> / <h3 id> headings.
//   2. An "Edit this page" link to the source on GitHub (resolved
//      from a <meta name="docs-edit-url"> set in the layout).
// We render the heading + nav unconditionally when there are at least
// 2 headings; the edit link renders even on short pages.
//
// Built programmatically (createElement + textContent) rather than via
// innerHTML so that heading text — which is authored by humans and may
// contain stray HTML metacharacters — is treated as text and never
// reinterpreted as markup (CodeQL: "DOM text reinterpreted as HTML").
function buildTOC() {
  if (!$content) return;

  const headings = $content.querySelectorAll('h2[id], h3[id]');
  const editUrl = (document.querySelector('meta[name="docs-edit-url"]') || {}).content || '';

  if (headings.length < 2 && !editUrl) return;

  const aside = document.createElement('aside');
  aside.className = 'toc-aside';
  aside.setAttribute('aria-label', 'On this page');

  const inner = document.createElement('div');
  inner.className = 'toc-inner';
  aside.appendChild(inner);

  // "On this page" + nav — only when there are enough headings.
  if (headings.length >= 2) {
    const heading = document.createElement('h2');
    heading.className = 'toc-heading';
    heading.textContent = 'On this page';
    inner.appendChild(heading);

    const nav = document.createElement('nav');
    nav.className = 'toc-nav';
    inner.appendChild(nav);

    for (const h of headings) {
      const a = document.createElement('a');
      a.className = 'toc-link' + (h.tagName === 'H3' ? ' toc-h3' : '');
      a.href = '#' + h.id;
      a.dataset.id = h.id;
      a.textContent = h.textContent;
      nav.appendChild(a);
    }

    aside.addEventListener('click', (e) => {
      const a = e.target.closest('.toc-link');
      if (!a) return;
      e.preventDefault();
      const target = document.getElementById(a.dataset.id);
      const reduceMotion = window.matchMedia?.('(prefers-reduced-motion: reduce)').matches;
      target?.scrollIntoView({ behavior: reduceMotion ? 'auto' : 'smooth', block: 'start' });
    });

    setupScrollSpy(headings, aside);
  }

  // "Edit this page" — only when an edit URL was provided.
  if (editUrl) {
    const actions = document.createElement('div');
    actions.className = 'toc-actions';

    const link = document.createElement('a');
    link.className = 'toc-action';
    link.href = editUrl;
    link.target = '_blank';
    link.rel = 'noopener';

    // Pencil icon (Lucide-style line). Reusing one literal here is
    // fine because no untrusted data is interpolated into it.
    link.insertAdjacentHTML('afterbegin',
      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor"' +
      ' stroke-width="2" stroke-linecap="round" stroke-linejoin="round"' +
      ' aria-hidden="true">' +
      '<path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/>' +
      '<path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/>' +
      '</svg>');

    const label = document.createElement('span');
    label.textContent = 'Edit this page';
    link.appendChild(label);

    actions.appendChild(link);
    inner.appendChild(actions);
  }

  const main = document.querySelector('.main');
  if (main) main.appendChild(aside);
}

function setupScrollSpy(headings, aside) {
  let currentActive = null;
  const visibleIds = new Set();
  const tocLinks = aside.querySelectorAll('.toc-link');

  const observer = new IntersectionObserver((entries) => {
    for (const entry of entries) {
      if (entry.isIntersecting) {
        visibleIds.add(entry.target.id);
      } else {
        visibleIds.delete(entry.target.id);
      }
    }

    // Pick the first heading (in DOM order) that is currently visible
    let nextActive = null;
    for (const h of headings) {
      if (visibleIds.has(h.id)) { nextActive = h.id; break; }
    }

    if (nextActive !== currentActive) {
      currentActive = nextActive;
      tocLinks.forEach(l => l.classList.remove('active'));
      if (currentActive) {
        aside.querySelector(`.toc-link[data-id="${currentActive}"]`)?.classList.add('active');
      }
    }
  }, { rootMargin: '-80px 0px -70% 0px', threshold: 0 });

  headings.forEach(h => observer.observe(h));
}

// ---------- Copy buttons ----------
function addCopyButtons() {
  if (!$content) return;
  const seen = new WeakSet();
  $content.querySelectorAll('pre, pre.highlight').forEach(pre => {
    if (seen.has(pre)) return;
    if (pre.querySelector('.copy-btn')) return;
    const parent = pre.parentElement;
    if (parent?.classList.contains('highlight') && parent.querySelector('.copy-btn')) return;
    seen.add(pre);

    const btn = document.createElement('button');
    btn.className = 'copy-btn';
    btn.textContent = 'Copy';
    btn.setAttribute('aria-label', 'Copy code to clipboard');
    btn.addEventListener('click', async () => {
      try {
        const text = pre.querySelector('code')?.textContent ?? pre.textContent;
        await navigator.clipboard.writeText(text);
        btn.textContent = 'Copied!';
        btn.classList.add('copied');
      } catch {
        btn.textContent = 'Failed';
      }
      setTimeout(() => { btn.textContent = 'Copy'; btn.classList.remove('copied'); }, 2000);
    });
    pre.style.position = 'relative';
    pre.appendChild(btn);
  });
}

// ---------- Search ----------
const searchIndex = [];

function buildSearchIndex() {
  const sidebarLinks = document.querySelectorAll('.sidebar-link');
  sidebarLinks.forEach(link => {
    const href = link.getAttribute('href');
    if (!href) return;
    const section = link.closest('.sidebar-section')?.querySelector('.sidebar-heading')?.textContent || '';
    searchIndex.push({
      title: link.textContent.trim(),
      url: href,
      section: section,
      searchText: `${link.textContent} ${section}`.toLowerCase(),
    });
  });

  if ($content) {
    const currentPath = window.location.pathname;
    const currentEntry = searchIndex.find(e =>
      currentPath.endsWith(e.url) || currentPath.endsWith(e.url.replace(/\/$/, ''))
    );
    if (currentEntry) {
      currentEntry.searchText += ' ' + $content.textContent.replace(/\s+/g, ' ').toLowerCase();
    }
  }
}

// WAI-ARIA dialog pattern: keep Tab/Shift-Tab cycling inside the modal
// instead of leaking focus into the page behind the overlay, and
// restore focus to whatever opened it on close.
let previouslyFocused = null;

function openSearch() {
  previouslyFocused = document.activeElement;
  $searchOverlay?.classList.add('active');
  if ($searchModal) {
    $searchModal.value = '';
    $searchModal.focus();
  }
  renderSearchResults('');
  document.addEventListener('keydown', trapSearchFocus);
}

function closeSearch() {
  $searchOverlay?.classList.remove('active');
  document.removeEventListener('keydown', trapSearchFocus);
  previouslyFocused?.focus();
  previouslyFocused = null;
}

function searchFocusables() {
  const results = $searchResults ? Array.from($searchResults.querySelectorAll('.search-result')) : [];
  return $searchModal ? [$searchModal, ...results] : results;
}

function trapSearchFocus(e) {
  if (e.key !== 'Tab') return;
  const focusables = searchFocusables();
  if (focusables.length === 0) return;
  const first = focusables[0];
  const last = focusables[focusables.length - 1];
  if (e.shiftKey && document.activeElement === first) {
    e.preventDefault();
    last.focus();
  } else if (!e.shiftKey && document.activeElement === last) {
    e.preventDefault();
    first.focus();
  }
}

// Arrow-key navigation: move real DOM focus between the query input and
// the result links (avoids needing an aria-activedescendant wiring).
function navigateSearchResults(e) {
  if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
  const results = $searchResults ? Array.from($searchResults.querySelectorAll('.search-result')) : [];
  if (results.length === 0) return;
  const idx = results.indexOf(document.activeElement);
  e.preventDefault();
  if (e.key === 'ArrowDown') {
    results[idx + 1] ? results[idx + 1].focus() : results[0].focus();
  } else if (idx <= 0) {
    $searchModal?.focus();
  } else {
    results[idx - 1].focus();
  }
}

function renderSearchResults(query) {
  if (!$searchResults) return;
  const q = query.toLowerCase().trim();

  const results = q === ''
    ? searchIndex.map(r => ({ ...r, matchType: 'browse' }))
    : searchIndex
        .map(r => {
          const titleMatch = r.title.toLowerCase().includes(q);
          const terms = q.split(/\s+/);
          const allTerms = terms.every(t => r.searchText.includes(t));
          if (!titleMatch && !allTerms) return null;
          const matchType = titleMatch ? 'title' : 'content';
          return { ...r, matchType };
        })
        .filter(Boolean)
        .sort((a, b) => {
          const order = { title: 0, content: 1 };
          return (order[a.matchType] ?? 2) - (order[b.matchType] ?? 2);
        });

  if (results.length === 0) {
    $searchResults.innerHTML = '<div class="search-empty">No results found</div>';
    return;
  }

  let html = '';
  let lastSection = '';
  for (const r of results) {
    if (r.section && r.section !== lastSection) {
      html += `<div class="search-result-group">${r.section}</div>`;
      lastSection = r.section;
    }
    html += `<a class="search-result" href="${r.url}" tabindex="0">
      <div class="search-result-title">${r.title}</div>
    </a>`;
  }
  $searchResults.innerHTML = html;

  $searchResults.querySelectorAll('.search-result').forEach(el => {
    el.addEventListener('click', () => closeSearch());
  });
}

// ---------- Event listeners ----------
$searchInput?.addEventListener('click', openSearch);
$searchModal?.addEventListener('input', (e) => renderSearchResults(e.target.value));
$searchModal?.addEventListener('keydown', navigateSearchResults);
$searchResults?.addEventListener('keydown', navigateSearchResults);
$searchOverlay?.addEventListener('click', (e) => { if (e.target === $searchOverlay) closeSearch(); });

document.addEventListener('keydown', (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key === 'k') {
    e.preventDefault();
    $searchOverlay?.classList.contains('active') ? closeSearch() : openSearch();
  }
  if (e.key === 'Escape') closeSearch();
});

// ---------- Sidebar scroll persistence ----------
function restoreSidebarScroll() {
  const sidebar = document.getElementById('sidebar');
  if (!sidebar) return;

  const saved = sessionStorage.getItem('sidebar-scroll');
  if (saved !== null) {
    sidebar.scrollTop = parseInt(saved, 10);
  }

  sidebar.querySelectorAll('a').forEach(link => {
    link.addEventListener('click', () => {
      sessionStorage.setItem('sidebar-scroll', sidebar.scrollTop);
    });
  });
}

// ---------- Demo video runtime fallback ----------
// The nested <img> inside <video> is native fallback CONTENT: browsers
// only render it when they don't recognize the <video> element at all,
// never on a decode/network failure of a supported element. That GIF
// is exactly the auto-playing, unpausable, infinitely-looping motion
// (WCAG 2.2.2) the video swap was meant to remove, so a decode/fetch
// failure must NOT fall back to it. Show the static poster frame
// instead — a still image carries no motion, so it needs no pause
// control — and leave the genuine no-<video>-support case to the
// browser's native fallback-content behavior above.
function handleVideoFallback() {
  document.querySelectorAll('.demo-container video').forEach(video => {
    video.addEventListener('error', () => {
      const poster = video.getAttribute('poster');
      if (!poster) return;
      const img = document.createElement('img');
      img.src = poster;
      img.alt = video.getAttribute('aria-label') || '';
      video.replaceWith(img);
    });
  });
}

// ---------- Bind buttons (no inline handlers) ----------
function bindButtons() {
  document.getElementById('theme-toggle')?.addEventListener('click', toggleTheme);
  document.querySelector('.sidebar-toggle')?.addEventListener('click', (e) => {
    const sidebar = document.getElementById('sidebar');
    const isOpen = sidebar?.classList.toggle('open');
    e.currentTarget.setAttribute('aria-expanded', String(!!isOpen));
  });
}

// ---------- Init ----------
initTheme();
restoreSidebarScroll();
buildSearchIndex();
buildTOC();
addCopyButtons();
bindButtons();
handleVideoFallback();
