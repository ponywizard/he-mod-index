'use strict';
const view = document.querySelector('#view');
const message = document.querySelector('#message');
const DEFAULT_MOD_IMAGE = '/default-mod.png';
let session = {user: null, csrf: ''};
let mods = [];
let routeVersion = 0;
let sort = {key: 'updated_at', direction: -1};
let filter = '';
let disposeEditor = null;
const expandedMods = new Set();
let postObserver = null;
let postLayoutFrame = 0;
const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]));
const date = value => new Date(value * 1000).toLocaleDateString(undefined, {year: 'numeric', month: 'short', day: 'numeric'});
const signed = n => n > 0 ? `+${n}` : String(n);
const link = (url, label) => `<a href="${esc(url)}" target="_blank" rel="noopener noreferrer">${esc(label)}</a>`;
function notice(text, success = false) {message.textContent = text; message.className = success ? 'success' : ''; message.hidden = !text;}
async function api(path, options = {}) {
  const headers = {'X-CSRF-Token': session.csrf, ...options.headers};
  if (options.body && typeof options.body !== 'string' && !(options.body instanceof Blob)) {
    options.body = JSON.stringify(options.body); headers['Content-Type'] = 'application/json';
  }
  const response = await fetch(path, {...options, headers, credentials: 'same-origin'});
  const data = await response.json();
  if (!response.ok) {
    const retry = Number(response.headers.get('Retry-After'));
    throw new Error((data.error || 'Request failed.') + (retry ? ` Retry in ${Math.ceil(retry / 60)} minute(s).` : ''));
  }
  return data;
}
function nav() {
  const u = session.user;
  document.querySelector('#nav').innerHTML = `<a href="/" data-nav>Index</a>${u ? `<a href="/submit" data-nav>Add mod</a><a href="/account" data-nav>Account</a>${u.role === 'admin' ? '<a href="/admin" data-nav>Admin</a>' : ''}<span class="identity">${esc(u.anonymous ? 'Anonymous' : u.username)}</span><button class="link-button" id="logout">Sign out</button>` : ''}`;
}
async function refreshSession() {session = await api('/api/session'); nav();}
function navigate(path, replace = false) {history[replace ? 'replaceState' : 'pushState']({}, '', path); render();}
function nextPath() {const path = new URLSearchParams(location.search).get('next'); return path && /^\/(?:mod\/\d+|submit|account|admin)(?:\?[^#]*)?$/.test(path) ? path : '/';}
function needUser() {if (session.user) return true; navigate(`/login?next=${encodeURIComponent(location.pathname + location.search)}`, true); return false;}
function badge(m) {
  if (m.approval === 'none') return '';
  const label = m.approval === 'current' ? `Approved by ${m.approved_by} for this release` : `Approved by ${m.approved_by} for an earlier release`;
  return `<span class="approval ${m.approval === 'stale' ? 'stale' : ''}" title="${esc(label)}" aria-label="${esc(label)}">✓</span>`;
}
function votes(m) {return `<div class="votes" data-mod="${m.id}"><button${session.user ? '' : ' disabled'} data-vote="1" aria-label="Upvote ${esc(m.name)}" aria-pressed="${m.my_vote === 1}">▲</button><span class="score" title="${m.upvotes} up / ${m.downvotes} down">${signed(m.rating)}</span><button${session.user ? '' : ' disabled'} data-vote="-1" aria-label="Downvote ${esc(m.name)}" aria-pressed="${m.my_vote === -1}">▼</button></div>`;}
function field(label, name, value = '', options = {}) {
  const id = `field-${name}`;
  const attrs = `${options.required ? ' required' : ''}${options.max ? ` maxlength="${options.max}"` : ''}${options.min ? ` minlength="${options.min}"` : ''}${options.autocomplete ? ` autocomplete="${options.autocomplete}"` : ''}`;
  return `<div class="field"><label for="${id}">${label}</label>${options.area ? `<textarea id="${id}" name="${name}" rows="${options.rows || 5}"${attrs}>${esc(value)}</textarea>` : `<input id="${id}" name="${name}" type="${options.type || 'text'}" value="${esc(value)}"${attrs}>`}${options.hint ? `<small>${options.hint}</small>` : ''}</div>`;
}
function wireForm(id, handler) {
  document.querySelector(id).addEventListener('submit', async event => {
    event.preventDefault(); notice('');
    const form = event.currentTarget, button = event.submitter;
    if (button) button.disabled = true;
    try {await handler(form);} catch (error) {notice(error.message);} finally {if (button) button.disabled = false;}
  });
}
function indexTable() {
  const terms = filter.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const filtered = mods.filter(m => terms.every(t => `${m.name} ${m.author} ${m.short_description} ${m.description} ${m.version} ${m.game_version} ${(m.downloads || []).map(d => d.filename).join(' ')}`.toLowerCase().includes(t)));
  filtered.sort((a, b) => {const av = a[sort.key], bv = b[sort.key]; const result = typeof av === 'string' ? av.localeCompare(bv) : av - bv; return result * sort.direction || b.id - a.id;});
  const cols = [['name', 'Name'], ['short_description', 'Description'], ['rating', 'Rating'], ['created_at', 'Added'], ['updated_at', 'Updated']];
  document.querySelector('#mod-count').textContent = `${filtered.length} / ${mods.length} mods`;
  document.querySelector('#mod-table').innerHTML = `<table class="index">
    <colgroup><col class="index-name"><col><col class="index-rating"><col class="index-date"><col class="index-date"></colgroup>
    <thead><tr>${cols.map(([key, label]) => `<th scope="col" aria-sort="${sort.key === key ? (sort.direction === 1 ? 'ascending' : 'descending') : 'none'}"><button data-sort="${key}">${label}${sort.key === key ? (sort.direction === 1 ? ' ↑' : ' ↓') : ''}</button></th>`).join('')}</tr></thead>
    <tbody>${filtered.length ? filtered.map(m => {
      const expanded = expandedMods.has(m.id);
      const version = `${m.version}${m.game_version ? ` · HE ${m.game_version}` : ''}`;
      return `<tr data-index-mod="${m.id}"${expanded ? ' class="is-expanded"' : ''}>
        <td class="mod-title-cell"><div class="name-content"><div class="name-heading"><a class="mod-name" href="/mod/${m.id}" data-nav title="${esc(m.name)}">${esc(m.name)}</a>${badge(m)}</div><a href="/mod/${m.id}" data-nav class="thumbnail-link" aria-label="View ${esc(m.name)}"><img class="thumbnail" src="${esc(m.thumbnail_url || DEFAULT_MOD_IMAGE)}" alt="" width="96" height="96" loading="lazy"></a></div></td>
        <td class="description-cell"><div class="post-preview" id="mod-post-${m.id}"><div class="post-header"><span class="post-author" title="${esc(m.author)}">${esc(m.author)}</span><span class="post-version" title="${esc(version)}">${esc(version)}</span></div><div class="post-subject">${esc(m.short_description)}</div><div class="post-body">${esc(m.description)}</div><button type="button" class="link-button post-more" data-expand-mod="${m.id}" aria-expanded="${expanded}" aria-controls="mod-post-${m.id}" hidden>${expanded ? 'Show less' : 'Show more'}</button></div></td>
        <td>${votes(m)}</td><td class="date">${date(m.created_at)}</td><td class="date">${date(m.updated_at)}</td>
      </tr>`;
    }).join('') : `<tr><td colspan="5" class="empty">${mods.length ? 'No matches.' : 'No mods yet.'}</td></tr>`}</tbody>
  </table>`;
  queuePostLayout();
}
function queuePostLayout() {
  cancelAnimationFrame(postLayoutFrame);
  postLayoutFrame = requestAnimationFrame(fitPostPreviews);
}
function fitPostPreviews() {
  postLayoutFrame = 0;
  for (const row of document.querySelectorAll('[data-index-mod]')) {
    const body = row.querySelector('.post-body');
    const subject = row.querySelector('.post-subject');
    const button = row.querySelector('.post-more');
    const expanded = row.classList.contains('is-expanded');
    body.style.webkitLineClamp = 'unset';
    body.style.maxHeight = '';
    button.textContent = expanded ? 'Show less' : 'Show more';
    button.setAttribute('aria-expanded', String(expanded));
    button.hidden = !expanded;
    if (expanded) continue;
    const overflows = body.scrollHeight > body.clientHeight + 1 || subject.scrollHeight > subject.clientHeight + 1;
    if (overflows) {
      button.hidden = false;
      const lineHeight = parseFloat(getComputedStyle(body).lineHeight);
      const lines = Math.max(1, Math.floor(body.clientHeight / lineHeight));
      body.style.webkitLineClamp = String(lines);
      body.style.maxHeight = `${lines * lineHeight}px`;
    }
  }
}
async function indexPage(current) {
  const loaded = await api('/api/mods'); if (current !== routeVersion) return; mods = loaded;
  view.innerHTML = `<div class="toolbar"><label for="filter">Filter</label><input id="filter" type="search" placeholder="Name, author, description…" value="${esc(filter)}" autocomplete="off"><span class="count" id="mod-count" role="status"></span></div><div class="scroll" id="mod-table"></div>`;
  document.querySelector('#filter').addEventListener('input', event => {filter = event.target.value; indexTable();});
  indexTable();
  if (typeof ResizeObserver !== 'undefined') {
    postObserver = new ResizeObserver(queuePostLayout);
    postObserver.observe(document.querySelector('#mod-table'));
  }
}
function authPage(register) {
  view.innerHTML = `<section class="panel narrow"><h2>${register ? 'Register' : 'Sign in'}</h2><form id="auth-form">${field('Username', 'username', '', {required: true, max: 32, autocomplete: 'username'})}${field('Password', 'password', '', {required: true, type: 'password', max: 128, min: register ? 10 : undefined, autocomplete: register ? 'new-password' : 'current-password', hint: register ? 'At least 10 characters.' : ''})}<button type="submit">${register ? 'Register' : 'Sign in'}</button></form></section>`;
  wireForm('#auth-form', async form => {await api(`/api/${register ? 'register' : 'login'}`, {method: 'POST', body: Object.fromEntries(new FormData(form))}); await refreshSession(); navigate(nextPath(), true);});
}
async function editorPage(id, current) {
  if (!needUser()) return;
  const m = id ? await api(`/api/mods/${id}`) : {}; if (current !== routeVersion) return;
  if (id && m.user_id !== session.user.id && session.user.role !== 'admin') throw new Error('You do not own this mod.');
  view.innerHTML = `<section class="panel editor"><h2>${id ? 'Edit mod' : 'Add mod'}</h2><form id="mod-form">
    ${field('Name', 'name', m.name, {required: true, max: 100})}
    <div class="fields">${field('Mod version', 'version', m.version, {required: true, max: 64})}${field('Home: Equestria version', 'game_version', m.game_version, {max: 64})}</div>
    <fieldset class="download-editor" id="download-editor"><legend>Downloads</legend><div data-download-rows></div><button type="button" data-add-link>Add link</button></fieldset>
    ${field('Repository URL', 'repository_url', m.repository_url, {type: 'url', max: 2048})}
    ${field('Short description', 'short_description', m.short_description, {required: true, max: 240})}
    ${field('Description', 'description', m.description, {area: true, rows: 9, max: 20000})}
    <fieldset class="image-editor" id="image-editor"><legend>Image (optional)</legend>
      <div class="field"><label for="image-file">Upload</label><input id="image-file" data-image-file type="file" accept="image/png,image/jpeg,image/gif"><small>PNG, JPEG, GIF · up to 4 MiB</small></div>
      <div class="field"><label for="image-url">Or import from URL</label><div class="image-url-row"><input id="image-url" data-image-url type="url" maxlength="2048"><button type="button" data-load-image>Load</button></div></div>
      <div data-crop-panel hidden><label for="crop-canvas">Thumbnail</label><div class="crop-layout"><canvas id="crop-canvas" width="256" height="256" tabindex="0" role="img" aria-label="Square thumbnail crop. Drag or use arrow keys to move."></canvas><div class="crop-controls"><label for="crop-zoom">Zoom</label><input id="crop-zoom" data-crop-zoom type="range" min="1" max="4" step="0.01" value="1"><label for="crop-x">Horizontal</label><input id="crop-x" data-crop-x type="range" min="0" max="100" step="0.1" value="50"><label for="crop-y">Vertical</label><input id="crop-y" data-crop-y type="range" min="0" max="100" step="0.1" value="50"><small>Drag to position.</small></div></div></div>
      <button type="button" class="link-button danger" data-remove-image hidden>Remove image</button>
    </fieldset>
    <div class="actions"><button type="submit">Save</button><a href="${id ? `/mod/${id}` : '/'}" data-nav>Cancel</a></div>
  </form></section>`;
  const downloads = new DownloadEditor(document.querySelector('#download-editor'), m.downloads || []);
  const image = new ImageEditor(document.querySelector('#image-editor'), m);
  disposeEditor = () => image.dispose();
  wireForm('#mod-form', async form => {
    await image.prepare();
    const data = new FormData(form);
    const input = Object.fromEntries(['name', 'version', 'game_version', 'repository_url', 'short_description', 'description'].map(key => [key, data.get(key)]));
    input.downloads = downloads.values(); input.image_url = image.imageURL();
    const saved = await api(id ? `/api/mods/${id}` : '/api/mods', {method: id ? 'PUT' : 'POST', body: input});
    // Retain a newly created listing ID if saving its image fails, so retrying
    // the form updates that listing rather than creating a duplicate.
    id = saved.id; image.mod = saved; history.replaceState({}, '', `/mod/${id}/edit`);
    try {await image.save(saved.id);} catch (error) {throw new Error(`Mod saved. Image save failed: ${error.message}`);}
    navigate(`/mod/${saved.id}`);
  });
}
async function detailPage(id, current) {
  const m = await api(`/api/mods/${id}`); if (current !== routeVersion) return;
  const u = session.user, owner = u && (m.user_id === u.id || u.role === 'admin');
  const hasScreenshot = m.image_url?.startsWith('/uploads/');
  const displayImage = hasScreenshot ? m.image_url : DEFAULT_MOD_IMAGE;
  const imageAlt = hasScreenshot ? `${m.name} screenshot` : 'Home: Equestria icon';
  document.title = `${m.name} — Home: Equestria mods`;
  view.innerHTML = `<section class="panel"><div class="detail-header"><h2>${esc(m.name)}${badge(m)}</h2><span>${esc(m.version)}</span></div><p class="detail-meta">${esc(m.author)} · Added ${date(m.created_at)} · Updated ${date(m.updated_at)}${m.game_version ? ` · Home: Equestria ${esc(m.game_version)}` : ''}</p><p>${esc(m.short_description)}</p><div class="actions">${m.repository_url ? link(m.repository_url, 'Repository') : ''}${m.source_url ? link(m.source_url, 'Source post') : ''}${votes(m)}${owner ? `<a href="/mod/${m.id}/edit" data-nav>Edit</a><button class="link-button danger" data-delete-mod="${m.id}">Remove</button>` : ''}</div><h3 class="section-title">Downloads</h3><ul class="downloads">${m.downloads.map(d => `<li>${link(d.url, d.filename)}</li>`).join('')}</ul><a href="${esc(displayImage)}" target="_blank" rel="noopener"><img class="screenshot" src="${esc(displayImage)}" alt="${esc(imageAlt)}" loading="lazy"></a>${m.description ? `<div class="description">${esc(m.description)}</div>` : ''}${m.approval !== 'none' ? `<p class="muted">${m.approval === 'current' ? 'Approved' : 'Earlier release approved'} by ${esc(m.approved_by)} · ${date(m.approved_at)}</p>` : ''}${u?.role === 'reviewer' ? `<div class="actions"><button data-approve="true" data-revision="${m.revision}" data-id="${m.id}">Approve this release</button>${m.approval !== 'none' ? `<button data-approve="false" data-revision="${m.revision}" data-id="${m.id}">Clear check</button>` : ''}</div>` : ''}</section>`;
}
async function accountPage(current) {
  if (!needUser()) return;
  const tokens = await api('/api/tokens'); if (current !== routeVersion) return;
  const mine = await api('/api/mods'); if (current !== routeVersion) return;
  const settings = `<h3 class="section-title">Settings</h3><section class="panel narrow"><form id="settings-form"><label class="checkline"><input type="checkbox" name="anonymous"${session.user.anonymous ? ' checked' : ''}>Anonymous display name</label><small>Show “Anonymous” on mods and reviews.</small><div class="actions"><button type="submit">Save settings</button></div></form></section>`;
  view.innerHTML = `<h2>${esc(session.user.username)}</h2>${settings}<h3 class="section-title">My mods</h3><div class="scroll"><table><thead><tr><th>Name</th><th>Version</th><th></th></tr></thead><tbody>${mine.filter(m => m.user_id === session.user.id).map(m => `<tr><td><a href="/mod/${m.id}" data-nav>${esc(m.name)}</a>${badge(m)}</td><td>${esc(m.version)}</td><td><a href="/mod/${m.id}/edit" data-nav>Edit</a></td></tr>`).join('') || '<tr><td colspan="3">No mods. <a href="/submit" data-nav>Add mod</a></td></tr>'}</tbody></table></div><h3 class="section-title">Upload tokens</h3><section class="panel editor"><form id="token-form">${field('Token name', 'name', '', {required: true, max: 80})}<button type="submit">Create token</button></form><div id="token-result"></div><div class="scroll"><table><thead><tr><th>Name</th><th>Expires</th><th></th></tr></thead><tbody>${tokens.map(t => `<tr><td>${esc(t.name)}</td><td class="date">${date(t.expires_at)}</td><td><button class="link-button danger" data-revoke="${t.id}">Revoke</button></td></tr>`).join('') || '<tr><td colspan="3">No tokens.</td></tr>'}</tbody></table></div><small>Tokens manage your mods and images.</small></section><h3 class="section-title">Change password</h3><section class="panel narrow"><form id="password-form">${field('Current password', 'current_password', '', {required: true, type: 'password', max: 128, autocomplete: 'current-password'})}${field('New password', 'password', '', {required: true, type: 'password', min: 10, max: 128, autocomplete: 'new-password'})}<button type="submit">Change password</button><small>Signs out all sessions and revokes upload tokens.</small></form></section>`;
  wireForm('#settings-form', async form => {
    session.user = await api('/api/settings', {method: 'PUT', body: {anonymous: form.elements.anonymous.checked}});
    nav();
    await accountPage(current);
    if (current === routeVersion) notice('Settings saved.', true);
  });
  wireForm('#token-form', async form => {
    const result = await api('/api/tokens', {method: 'POST', body: Object.fromEntries(new FormData(form))});
    await accountPage(current);
    document.querySelector('#token-result').innerHTML = `<div class="token-result"><p>Copy this token now. It is only shown once.</p><code>${esc(result.token)}</code></div>`;
  });
  wireForm('#password-form', async form => {await api('/api/password', {method: 'PUT', body: Object.fromEntries(new FormData(form))}); await refreshSession(); navigate('/login'); notice('Password changed. Sign in again.', true);});
}
async function adminPage(current) {
  if (!needUser()) return;
  if (session.user.role !== 'admin') throw new Error('Admin access required.');
  const [users, allMods, imports, importErrors] = await Promise.all([api('/api/admin/users'), api('/api/mods'), api('/api/admin/imports'), api('/api/admin/import-errors')]); if (current !== routeVersion) return;
  const usernames = new Map(users.map(u => [u.id, u.username]));
  view.innerHTML = `<h2>Admin</h2>${importErrors.length ? `<h3 class="section-title">Import errors</h3>${importErrors.map(p => `<section class="panel"><p>${link(p.source.url, `Post ${p.id}`)} · ${esc(p.reason)}</p><button data-import-retry="${p.id}">Retry next poll</button> <button class="link-button danger" data-import-dismiss="${p.id}">Dismiss</button></section>`).join('')}` : ''}<h3 class="section-title">Pending imports</h3>${imports.map(c => `<section class="panel" data-import="${c.id}"><h3>${esc(c.entry.name)} · ${esc(c.entry.version)}</h3><p>${esc(c.reason)}</p><p>${esc(c.entry.short_description)}</p><details><summary>Description and source</summary><div class="description">${esc(c.entry.description)}</div><p>${link(c.source.url, 'Source post')}</p><div class="description">${esc(c.source.text)}</div></details><ul>${c.entry.downloads.map(d => `<li>${link(d.url, d.filename)}</li>`).join('')}</ul><label>Publish to <select data-import-target><option value="0" data-revision="0">New listing</option>${allMods.map(m => `<option value="${m.id}" data-revision="${m.revision}">${esc(m.name)} · ${esc(m.version)}</option>`).join('')}</select></label><div class="actions"><button data-import-publish="${c.id}">Publish unreviewed</button><button class="link-button danger" data-import-reject="${c.id}">Dismiss</button></div></section>`).join('') || '<p>No pending imports.</p>'}<h3 class="section-title">Users</h3><div class="scroll"><table><thead><tr><th>Username</th><th>Role</th><th>Registered</th><th>Mods</th><th>Votes</th><th></th></tr></thead><tbody>${users.map(u => `<tr><td>${esc(u.username)}</td><td>${esc(u.role)}</td><td class="date">${date(u.created_at)}</td><td>${u.mods}</td><td>${u.votes}</td><td>${u.id !== session.user.id ? `<button class="link-button danger" data-delete-user="${u.id}" data-name="${esc(u.username)}">Remove</button>` : ''}</td></tr>`).join('')}</tbody></table></div><h3 class="section-title">Mods</h3><div class="scroll"><table><thead><tr><th>Name</th><th>Author</th><th></th></tr></thead><tbody>${allMods.map(m => `<tr><td><a href="/mod/${m.id}" data-nav>${esc(m.name)}</a></td><td>${esc(usernames.get(m.user_id) || m.author)}</td><td><button class="link-button danger" data-delete-mod="${m.id}">Remove</button></td></tr>`).join('') || '<tr><td colspan="3">No mods.</td></tr>'}</tbody></table></div>`;
}
async function render() {
  if (postObserver) {postObserver.disconnect(); postObserver = null;}
  cancelAnimationFrame(postLayoutFrame);
  if (disposeEditor) {disposeEditor(); disposeEditor = null;}
  const current = ++routeVersion; notice(''); document.title = 'Mods — Home: Equestria';
  view.innerHTML = '<p>Loading…</p>';
  const path = location.pathname;
  try {
    const edit = path.match(/^\/mod\/(\d+)\/edit$/), detail = path.match(/^\/mod\/(\d+)$/);
    if (path === '/') await indexPage(current);
    else if (path === '/login' || path === '/register') authPage(path === '/register');
    else if (path === '/submit' || edit) await editorPage(edit?.[1], current);
    else if (detail) await detailPage(detail[1], current);
    else if (path === '/account') await accountPage(current);
    else if (path === '/admin') await adminPage(current);
    else view.innerHTML = '<p>Page not found.</p><a href="/" data-nav>Index</a>';
  } catch (error) {if (current === routeVersion) {view.innerHTML = ''; notice(error.message);}}
}
document.addEventListener('click', async event => {
  const a = event.target.closest('a[data-nav]');
  if (a && !event.ctrlKey && !event.metaKey && !event.shiftKey && !event.altKey && event.button === 0) {event.preventDefault(); navigate(a.getAttribute('href')); return;}
  const button = event.target.closest('button'); if (!button) return;
  try {
    if (button.id === 'logout') {await api('/api/logout', {method: 'POST'}); await refreshSession(); navigate('/');}
    if (button.dataset.sort) {sort = {key: button.dataset.sort, direction: sort.key === button.dataset.sort ? -sort.direction : (button.dataset.sort === 'name' ? 1 : -1)}; indexTable();}
    if (button.dataset.expandMod) {
      const id = Number(button.dataset.expandMod);
      if (expandedMods.has(id)) expandedMods.delete(id); else expandedMods.add(id);
      button.closest('[data-index-mod]').classList.toggle('is-expanded', expandedMods.has(id));
      fitPostPreviews();
    }
    if (button.dataset.vote) {
      if (!session.user) {navigate(`/login?next=${encodeURIComponent(location.pathname)}`); return;}
      button.disabled = true;
      const id = button.closest('[data-mod]').dataset.mod;
      const requested = Number(button.dataset.vote), value = button.getAttribute('aria-pressed') === 'true' ? 0 : requested;
      const result = await api(`/api/mods/${id}/vote`, {method: 'PUT', body: {value}});
      if (location.pathname === '/') {mods = mods.map(m => m.id === result.id ? result : m); indexTable();} else await render();
    }
    if (button.dataset.importRetry || button.dataset.importDismiss) {
      button.disabled = true;
      await api(`/api/admin/import-errors/${button.dataset.importRetry || button.dataset.importDismiss}`, {method: 'POST', body: {action: button.dataset.importRetry ? 'retry' : 'dismiss'}});
      await render();
    }
    if (button.dataset.importPublish || button.dataset.importReject) {
      button.disabled = true;
      const panel = button.closest('[data-import]'), choice = panel.querySelector('[data-import-target]').selectedOptions[0];
      const result = await api(`/api/admin/imports/${panel.dataset.import}`, {method: 'POST', body: {action: button.dataset.importReject ? 'reject' : 'publish', target_id: Number(choice.value), revision: Number(choice.dataset.revision)}});
      await render(); if (result.warning) notice(result.warning);
    }
    if (button.dataset.approve) {button.disabled = true; await api(`/api/mods/${button.dataset.id}/approval`, {method: 'PUT', body: {approved: button.dataset.approve === 'true', revision: Number(button.dataset.revision)}}); await render();}
    if (button.dataset.revoke) {await api(`/api/tokens/${button.dataset.revoke}`, {method: 'DELETE'}); await render();}
    if (button.dataset.deleteMod && confirm('Remove this mod and its votes?')) {await api(`/api/mods/${button.dataset.deleteMod}`, {method: 'DELETE'}); if (location.pathname === '/admin') await render(); else navigate('/');}
    if (button.dataset.deleteUser && confirm(`Remove ${button.dataset.name}, all their mods, and all their votes?`)) {await api(`/api/admin/users/${button.dataset.deleteUser}`, {method: 'DELETE'}); await render();}
  } catch (error) {notice(error.message);} finally {button.disabled = false;}
});
window.addEventListener('popstate', render);
window.addEventListener('resize', () => {if (location.pathname === '/') queuePostLayout();});
refreshSession().then(render).catch(error => {view.innerHTML = ''; notice(error.message);});

api('/api/site').then(site => {const a = document.querySelector('#current-thread'); if (a && /^https:\/\/boards\.4chan\.org\/mlp\/thread\/\d+$/.test(site.current_thread)) a.href = site.current_thread;}).catch(() => {});
