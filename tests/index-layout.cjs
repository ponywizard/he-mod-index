// Optional browser regression check. Uses Node built-ins and an installed Chromium.
// All requests are fulfilled from fixtures; no running site or live data is used.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {spawn} = require('node:child_process');
const {StringDecoder} = require('node:string_decoder');

const root = path.resolve(__dirname, '..');
const screenshotDirectory = process.env.SCREENSHOT_DIR || (process.argv.includes('--screenshots') ? path.join(os.tmpdir(), 'he-mod-index-layout-previews') : '');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'he-mod-index-browser-'));
const args = ['--headless', '--disable-gpu', '--disable-background-networking', '--disable-component-update', '--disable-sync', '--no-first-run', '--no-default-browser-check', '--remote-debugging-pipe', `--user-data-dir=${profile}`];
if (process.env.CHROMIUM_NO_SANDBOX === '1') args.push('--no-sandbox');
const browser = spawn(process.env.CHROMIUM_BIN || 'chromium', args, {stdio: ['ignore', 'ignore', 'pipe', 'pipe', 'pipe']});
const pending = new Map();
const errors = [];
let sequence = 0, input = '', stderr = '', closing = false;
let handleEvent = () => {};
browser.stderr.on('data', chunk => {stderr = (stderr + chunk).slice(-4000);});
function failPending(error) {for (const p of pending.values()) {clearTimeout(p.timer); p.reject(error);} pending.clear();}
browser.on('error', failPending);
browser.on('exit', code => failPending(new Error(`Chromium exited (${code}). ${stderr}`)));
const decoder = new StringDecoder('utf8');
browser.stdio[4].on('data', chunk => {
  input += decoder.write(chunk);
  let end;
  while ((end = input.indexOf('\0')) >= 0) {
    const data = input.slice(0, end); input = input.slice(end + 1); if (!data) continue;
    const message = JSON.parse(data), p = pending.get(message.id);
    if (p) {pending.delete(message.id); clearTimeout(p.timer); if (message.error) p.reject(new Error(JSON.stringify(message.error))); else p.resolve(message.result);}
    else handleEvent(message);
  }
});
function call(method, params = {}, sessionId) {
  return new Promise((resolve, reject) => {
    if (browser.exitCode !== null || browser.signalCode) return reject(new Error(stderr || 'Chromium is not running.'));
    const id = ++sequence;
    const timer = setTimeout(() => {pending.delete(id); reject(new Error(`CDP timeout: ${method}. ${stderr}`));}, 15000);
    pending.set(id, {resolve, reject, timer});
    browser.stdio[3].write(JSON.stringify({id, method, params, ...(sessionId ? {sessionId} : {})}) + '\0');
  });
}
const timestamp = 1794700800;
const mods = [
  {id: 1, name: 'Breeze TTS', author: 'ponywizard', short_description: 'Companion voices and speech', description: ('Installation instructions and voice configuration.\n').repeat(45) + '<img src=x onerror="window.unsafe=true"> unique-search-word', thumbnail_url: '/uploads/test.jpg', rating: 12},
  {id: 2, name: 'A mod with a deliberately long name that wraps beyond the first line', author: 'Anonymous', short_description: ('A longer subject with useful details ').repeat(12), description: 'A short body.', thumbnail_url: '', rating: -2},
  {id: 3, name: 'Simple mod', author: 'mod_author', short_description: 'A short subject', description: 'A short description that fits.', thumbnail_url: '/uploads/test.jpg', rating: 0},
  {id: 4, name: 'No image', author: 'Anonymous', short_description: 'No long description', description: '', thumbnail_url: '', rating: 4},
].map(m => ({user_id: 1, revision: 1, version: '1.2.0', game_version: '0.3.0-maredev', created_at: timestamp, updated_at: timestamp + m.id, approval: 'none', upvotes: 0, downvotes: 0, my_vote: 0, downloads: [{filename: 'mod.pck', url: 'https://example.org/mod.pck'}], image_url: m.thumbnail_url ? '/uploads/test.jpg' : '', ...m}));
const gallery = [1, 2, 3].map(n => ({image_url: `/uploads/gallery-${n}.jpg`, thumbnail_url: `/uploads/gallery-${n}-thumb.jpg`, source_url: `https://desuarchive.org/mlp/thread/40000000/#4000000${n}`, version: `${n}.0`, description: n === 1 ? '<img src=x onerror="window.unsafe=true"> Source image' : `Screenshot for version ${n}.0`, posted_at: timestamp + n}));
const image = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jGpcAAAAASUVORK5CYII=', 'base64');
const files = new Map(['index.html', 'app.js', 'editor.js', 'style.css', 'default-mod.png'].map(name => [name, fs.readFileSync(path.join(root, 'web', name))]));
let session;
let catalog = mods;
let fixtureUser = {id: 1, username: 'owner', role: 'user'};
let pendingImport = true;
let publishedImport = null;
const importFixture = {id: 10, reason: 'Confirm the matching listing.', source: {id: 2, author: "Anonymous", image: "https://example.org/source.png", time: timestamp - 1000, url: 'https://desuarchive.org/mlp/thread/1/#2', text: '<img src=x onerror="window.unsafe=true"> Source post'}, entry: {name: 'Imported mod', version: '2.0', short_description: 'A discovered mod.', description: 'Install the package.', downloads: [{filename: 'mod.pck', url: 'https://example.org/mod.pck'}]}};
async function evaluate(expression) {
  const result = await call('Runtime.evaluate', {expression, returnByValue: true, awaitPromise: true}, session);
  if (result.exceptionDetails) throw new Error(JSON.stringify(result.exceptionDetails));
  return result.result.value;
}
async function until(expression) {
  const deadline = Date.now() + 10000;
  while (!await evaluate(expression)) {if (Date.now() > deadline) throw new Error(`UI timeout: ${expression}`); await new Promise(resolve => setTimeout(resolve, 30));}
}
async function screenshot(name) {
  if (!screenshotDirectory) return;
  fs.mkdirSync(screenshotDirectory, {recursive: true});
  const result = await call('Page.captureScreenshot', {format: 'png'}, session);
  fs.writeFileSync(path.join(screenshotDirectory, name), Buffer.from(result.data, 'base64'));
}
(async () => {
  const target = await call('Target.createTarget', {url: 'about:blank'});
  session = (await call('Target.attachToTarget', {targetId: target.targetId, flatten: true})).sessionId;
  handleEvent = message => {
    if (message.method === 'Runtime.exceptionThrown') errors.push(JSON.stringify(message.params.exceptionDetails));
    if (message.method === 'Log.entryAdded' && message.params.entry.level === 'error') errors.push(message.params.entry.text);
    if (message.method !== 'Fetch.requestPaused') return;
    const request = message.params, url = new URL(request.request.url);
    let body, type = 'application/json', status = 200;
    if (['/', '/login', '/admin'].includes(url.pathname)) {body = files.get('index.html'); type = 'text/html';}
    else if (files.has(url.pathname.slice(1))) {body = files.get(url.pathname.slice(1)); type = url.pathname.endsWith('.png') ? 'image/png' : url.pathname.endsWith('.css') ? 'text/css' : 'text/javascript';}
    else if (url.pathname === '/api/session') body = Buffer.from(JSON.stringify({user: fixtureUser, csrf: 'test'}));
    else if (url.pathname === '/api/site') body = Buffer.from(JSON.stringify({current_thread: 'https://boards.4chan.org/mlp/thread/43715264'}));
    else if (url.pathname === '/api/admin/users') body = Buffer.from(JSON.stringify([{...fixtureUser, created_at: timestamp, mods: mods.length, votes: 0}]));
    else if (url.pathname === '/api/admin/import-errors') body = Buffer.from('[]');
    else if (url.pathname === '/api/admin/imports') body = Buffer.from(JSON.stringify(pendingImport ? [importFixture] : []));
    else if (url.pathname === '/api/admin/imports/10') {publishedImport = JSON.parse(request.request.postData); pendingImport = false; body = Buffer.from('{"id":1,"warning":""}');}
    else if (/^\/api\/mods\/\d+\/images$/.test(url.pathname)) body = Buffer.from(JSON.stringify(url.pathname.includes('/1/') ? gallery : url.pathname.includes('/3/') ? [gallery[0]] : []));
    else if (/^\/api\/mods\/\d+\/history$/.test(url.pathname)) body = Buffer.from(JSON.stringify([{version: '1.0', posted_at: timestamp, source_url: 'https://desuarchive.org/mlp/thread/1/#2', source: importFixture.source, short_description: 'Original release', description: '<img src=x onerror="window.unsafe=true"> Historical notes', downloads: mods[0].downloads}]));
    else if (url.pathname === '/api/mods') body = Buffer.from(JSON.stringify(catalog));
    else if (/^\/api\/mods\/\d+$/.test(url.pathname)) body = Buffer.from(JSON.stringify(catalog.find(m => m.id === Number(url.pathname.split('/').pop()))));
    else if (/^\/api\/source-posts\/\d+\/image$/.test(url.pathname) || /^\/uploads\//.test(url.pathname)) {body = image; type = 'image/png';}
    else {body = Buffer.alloc(0); status = url.pathname === '/favicon.ico' ? 204 : 404;}
    call('Fetch.fulfillRequest', {requestId: request.requestId, responseCode: status, responseHeaders: [{name: 'Content-Type', value: type}, {name: 'Content-Security-Policy', value: "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' blob:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"}], body: body.toString('base64')}, message.sessionId).catch(error => {if (!closing) errors.push(error.message);});
  };
  for (const method of ['Page.enable', 'Runtime.enable', 'Log.enable']) await call(method, {}, session);
  await call('Fetch.enable', {patterns: [{urlPattern: '*'}]}, session);
  await call('Emulation.setDeviceMetricsOverride', {width: 1200, height: 850, deviceScaleFactor: 1, mobile: false}, session);
  await call('Page.navigate', {url: 'http://he-mod-index.test/'}, session);
  await until("document.querySelectorAll('[data-index-mod]').length === 4 && !document.querySelector('[data-index-mod=\"1\"] .post-more').hidden");
  await until("document.querySelector('[data-index-mod=\"4\"] .thumbnail').naturalWidth === 256");
  assert.equal(await evaluate("document.querySelector('[data-index-mod=\"4\"] .thumbnail').getAttribute('src')"), '/default-mod.png');
  assert.equal(await evaluate("document.querySelector('[data-index-mod=\"3\"] .thumbnail').getAttribute('src')"), '/uploads/test.jpg');
  assert.deepEqual(await evaluate("[...document.querySelectorAll('thead button')].map(el => el.textContent.replace(/[ ↑↓]/g, ''))"), ['Description', 'Rating', 'Added', 'Updated']);
  assert.equal(await evaluate("document.querySelector('thead th').textContent"), '');
  assert.equal(await evaluate("document.querySelectorAll('.mod-image-cell .mod-name').length"), 0);
  assert.equal(await evaluate("document.querySelectorAll('.description-cell .mod-name').length"), 4);
  assert.equal(await evaluate("document.querySelector('thead button').dataset.sort"), 'name');
  const heights = await evaluate("[...document.querySelectorAll('[data-index-mod]')].map(row => row.getBoundingClientRect().height)");
  assert(Math.max(...heights) - Math.min(...heights) < 1, `Description changed row height: ${heights}`);
  assert(heights[0] < 180, `Collapsed rows are too tall: ${heights}`);
  assert.equal(await evaluate("document.querySelector('.mod-image-cell .post-author')"), null);
  assert.equal(await evaluate("getComputedStyle(document.querySelector('.post-author')).color"), 'rgb(17, 119, 67)');
  assert.equal(await evaluate("getComputedStyle(document.querySelector('.post-subject')).fontWeight"), '700');
  assert(await evaluate("document.querySelector('.post-version').getBoundingClientRect().left > document.querySelector('.post-author').getBoundingClientRect().right"));
  assert.equal(await evaluate("document.querySelector('[data-index-mod=\"3\"] .post-more').hidden"), true);
  assert.equal(await evaluate("document.querySelector('[data-index-mod=\"2\"] .post-more').hidden"), false);
  assert(await evaluate("{const body = document.querySelector('[data-index-mod=\"1\"] .post-body'); body.clientHeight <= parseInt(getComputedStyle(body).webkitLineClamp) * parseFloat(getComputedStyle(body).lineHeight);}"), 'Text is visible below the clamped last line');
  assert.equal(await evaluate('window.unsafe'), undefined);
  await screenshot('index-desktop.png');
  const collapsed = await evaluate("document.querySelector('[data-index-mod=\"1\"]').getBoundingClientRect().height");
  await evaluate("document.querySelector('[data-expand-mod=\"1\"]').click()");
  assert(await evaluate(`document.querySelector('[data-index-mod="1"]').getBoundingClientRect().height > ${collapsed + 40}`));
  assert.equal(await evaluate("document.querySelector('[data-expand-mod=\"1\"]').getAttribute('aria-expanded')"), 'true');
  await evaluate("document.querySelector('[data-expand-mod=\"1\"]').click()");
  assert.equal(await evaluate("document.querySelector('[data-index-mod=\"1\"]').getBoundingClientRect().height"), collapsed);
  await evaluate("document.querySelector('[data-expand-mod=\"2\"]').click()");
  assert(await evaluate("{const subject = document.querySelector('[data-index-mod=\"2\"] .post-subject'); subject.clientHeight >= subject.scrollHeight - 1;}"));
  await evaluate("document.querySelector('[data-expand-mod=\"2\"]').click()");
  await evaluate("document.querySelector('[data-sort=\"name\"]').click()");
  assert.equal(await evaluate("document.querySelector('tbody tr').dataset.indexMod"), '2');
  await evaluate("document.querySelector('[data-sort=\"rating\"]').click()");
  assert.equal(await evaluate("document.querySelector('tbody tr').dataset.indexMod"), '1');
  await evaluate("{const input = document.querySelector('#filter'); input.value = 'unique-search-word'; input.dispatchEvent(new Event('input'));}");
  assert.equal(await evaluate("document.querySelectorAll('[data-index-mod]').length"), 1);
  await evaluate("document.querySelector('#filter').value = ''; document.querySelector('#filter').dispatchEvent(new Event('input'))");
  await call('Emulation.setDeviceMetricsOverride', {width: 390, height: 850, deviceScaleFactor: 1, mobile: true}, session);
  await until("document.querySelector('#mod-table').clientWidth < 400");
  await new Promise(resolve => setTimeout(resolve, 100));
  const mobile = await evaluate("({page: document.documentElement.scrollWidth, width: innerWidth, table: document.querySelector('#mod-table').scrollWidth, container: document.querySelector('#mod-table').clientWidth, heights: [...document.querySelectorAll('[data-index-mod]')].map(row => row.getBoundingClientRect().height)})");
  assert(mobile.page <= mobile.width + 1, `Page overflows on mobile: ${JSON.stringify(mobile)}`);
  assert(mobile.table > mobile.container, 'Table does not scroll independently on mobile');
  assert(Math.max(...mobile.heights) - Math.min(...mobile.heights) < 1, `Mobile description expanded rows: ${mobile.heights}`);
  await screenshot('index-mobile.png');
  await evaluate("document.querySelector('a.mod-name[href=\"/mod/4\"]').click()");
  await until("Boolean(document.querySelector('.detail-header'))");
  assert.equal(await evaluate("document.querySelectorAll('#view img:not([data-source-image])').length"), 0);
  assert.equal(await evaluate("document.querySelector('.mod-gallery')"), null);
  assert.equal(await evaluate("document.querySelector('.release-history summary').textContent"), 'Post history (1)');
  await evaluate("document.querySelector('.release-history summary').click()");
  assert.equal(await evaluate("document.querySelector('.release-history .description').textContent"), '<img src=x onerror="window.unsafe=true"> Historical notes');
  assert.equal(await evaluate('window.unsafe'), undefined);
  // Multi-image gallery: bottom placement, captions, keyboard navigation and slideshow.
  await evaluate("document.querySelector('nav a[href=\"/\"]').click()");
  await until("Boolean(document.querySelector('a.mod-name[href=\"/mod/1\"]'))");
  await evaluate("document.querySelector('a.mod-name[href=\"/mod/1\"]').click()");
  await until("document.querySelectorAll('[data-gallery-image]').length === 3");
  assert(await evaluate("document.querySelector('#view > .panel').lastElementChild.classList.contains('mod-gallery')"));
  assert.equal(await evaluate("document.querySelectorAll('#view img[src=\"/default-mod.png\"]').length"), 0);
  assert.equal(await evaluate("document.querySelector('.gallery-grid figcaption p').textContent"), gallery[0].description);
  assert.equal(await evaluate("document.querySelector('.gallery-grid .image-source a').href"), gallery[0].source_url);
  assert.equal(await evaluate("document.querySelector('.gallery-grid .image-source span').textContent"), 'Version 1.0');
  assert(await evaluate("document.documentElement.scrollWidth <= innerWidth + 1"));
  await evaluate("document.querySelector('[data-gallery-image=\"0\"]').click()");
  assert(await evaluate("document.querySelector('.image-viewer').open"));
  assert.equal(await evaluate("document.querySelector('.viewer-position').textContent"), '1 / 3');
  await evaluate("document.querySelector('[data-viewer-next]').click()");
  assert.equal(await evaluate("document.querySelector('.viewer-position').textContent"), '2 / 3');
  await call('Input.dispatchKeyEvent', {type: 'keyDown', key: 'ArrowLeft', code: 'ArrowLeft', windowsVirtualKeyCode: 37}, session);
  assert.equal(await evaluate("document.querySelector('.viewer-position').textContent"), '1 / 3');
  await evaluate("document.querySelector('[data-viewer-previous]').click()");
  assert.equal(await evaluate("document.querySelector('.viewer-position').textContent"), '3 / 3');
  await evaluate("document.querySelector('[data-viewer-play]').click()");
  assert.equal(await evaluate("document.querySelector('[data-viewer-play]').getAttribute('aria-pressed')"), 'true');
  await until("document.querySelector('.viewer-position').textContent === '1 / 3'");
  await evaluate("document.querySelector('[data-viewer-play]').click()");
  assert.equal(await evaluate("document.querySelector('[data-viewer-play]').textContent"), 'Start slideshow');
  await screenshot('gallery-mobile-viewer.png');
  await call('Input.dispatchKeyEvent', {type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27}, session);
  await call('Input.dispatchKeyEvent', {type: 'keyUp', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27}, session);
  await until("!document.querySelector('.image-viewer').open && !document.body.classList.contains('viewer-open')");
  assert.equal(await evaluate("document.activeElement.dataset.galleryImage"), '0');
  assert.equal(await evaluate('window.unsafe'), undefined);
  await call('Emulation.setDeviceMetricsOverride', {width: 1200, height: 850, deviceScaleFactor: 1, mobile: false}, session);
  await evaluate("document.querySelector('.mod-gallery').scrollIntoView()");
  await screenshot('gallery-desktop.png');
  await evaluate("document.querySelector('[data-gallery-image=\"1\"]').click()");
  await screenshot('gallery-desktop-viewer.png');
  await evaluate("document.querySelector('[data-viewer-play]').click(); document.querySelector('nav a[href=\"/\"]').click()");
  await until("Boolean(document.querySelector('a.mod-name[href=\"/mod/3\"]'))");
  assert.equal(await evaluate("document.querySelector('.image-viewer')"), null);
  assert.equal(await evaluate("document.body.classList.contains('viewer-open')"), false);
  await evaluate("document.querySelector('a.mod-name[href=\"/mod/3\"]').click()");
  await until("document.querySelectorAll('[data-gallery-image]').length === 1");
  await evaluate("document.querySelector('[data-gallery-image]').click()");
  assert.equal(await evaluate("document.querySelector('[data-viewer-play]').hidden && document.querySelector('[data-viewer-next]').hidden"), true);
  await evaluate("document.querySelector('[data-viewer-close]').click()");
  await evaluate("document.querySelector('nav a[href=\"/submit\"]').click()");
  await until("Boolean(document.querySelector('#mod-form'))");
  assert.equal(await evaluate("document.querySelector('#image-editor legend').textContent"), 'Image (optional)');
  await evaluate("document.querySelector('#field-name').value = 'No picture'; document.querySelector('#field-version').value = '1.0'; document.querySelector('#field-short_description').value = 'A mod without a picture'; document.querySelector('[data-filename]').value = 'mod.pck'; document.querySelector('[data-url]').value = 'https://example.org/mod.pck';");
  assert(await evaluate("document.querySelector('#mod-form').checkValidity()"), 'The add form requires an image');
  fixtureUser = null;
  await call('Page.navigate', {url: 'http://he-mod-index.test/'}, session);
  await until("document.querySelectorAll('[data-index-mod]').length === 4");
  assert.equal(await evaluate("document.querySelectorAll('nav a[href=\"/login\"], nav a[href=\"/register\"], nav a[href=\"/submit\"]').length"), 0);
  assert(await evaluate("[...document.querySelectorAll('[data-vote]')].every(b => b.disabled)"));
  assert.equal(await evaluate("document.querySelector('link[rel=alternate]').getAttribute('href')"), '/feed.xml');
  await call('Page.navigate', {url: 'http://he-mod-index.test/login'}, session);
  await until("Boolean(document.querySelector('#auth-form'))");
  assert.equal(await evaluate("document.querySelector('a[href=\"/register\"]')"), null);
  fixtureUser = {id: 1, username: 'operator', role: 'admin'};
  await call('Page.navigate', {url: 'http://he-mod-index.test/admin'}, session);
  await until("Boolean(document.querySelector('[data-import-publish]'))");
  assert.equal(await evaluate('window.unsafe'), undefined);
  assert.equal(await evaluate("document.querySelector('[data-import-publish]').textContent"), 'Create listing');
  assert.equal(await evaluate("document.querySelector('[data-import-history]').hidden"), true);
  assert.equal(await evaluate("document.querySelector('[data-import]').textContent.includes('Publish unreviewed')"), false);
  assert.equal(await evaluate("document.querySelector('[data-import]').textContent.includes('Invalid Date')"), false);
  await evaluate("const select = document.querySelector('[data-import-target]'); select.value = '1'; select.dispatchEvent(new Event('change', {bubbles:true}))");
  assert.equal(await evaluate("document.querySelector('[data-import-publish]').textContent"), 'Update listing');
  assert.equal(await evaluate("document.querySelector('[data-import-history]').hidden"), false);
  assert.equal(await evaluate("document.querySelector('[data-import-current] a').getAttribute('href')"), '/mod/1');
  assert.equal(await evaluate("document.querySelector('[data-import] > .source-post .source-body').textContent"), importFixture.source.text);
  assert.equal(await evaluate("document.querySelector('[data-source-image]').getAttribute('src')"), '/api/source-posts/2/image');
  assert.equal(await evaluate("document.querySelector('.source-post').closest('details')"), null);
  await screenshot('imports-desktop.png');
  await call('Emulation.setDeviceMetricsOverride', {width: 390, height: 844, deviceScaleFactor: 1, mobile: true}, session);
  assert(await evaluate("document.querySelector('[data-import-target]').getBoundingClientRect().right <= document.querySelector('[data-import]').getBoundingClientRect().right"), 'Import selector overflows its panel on mobile');
  await screenshot('imports-mobile.png');
  await evaluate("document.querySelector('[data-import-publish]').click()");
  await until("!document.querySelector('[data-import-publish]')");
  assert.deepEqual(publishedImport, {action: 'publish', target_id: 1, revision: 1});
  for (const [action, selector, target] of [['history', 'history', 1], ['publish', 'publish', 0], ['reject', 'reject', 0]]) {
    pendingImport = true;
    await call('Page.navigate', {url: 'http://he-mod-index.test/admin'}, session);
    await until("Boolean(document.querySelector('[data-import-publish]'))");
    await evaluate(`document.querySelector('[data-import-target]').value = '${target}'; document.querySelector('[data-import-target]').dispatchEvent(new Event('change', {bubbles:true})); document.querySelector('[data-import-${selector}]').click()`);
    await until("!document.querySelector('[data-import-publish]')");
    assert.deepEqual(publishedImport, {action, target_id: target, revision: target ? 1 : 0});
    assert.equal(await evaluate("document.querySelector('#message').className"), 'success');
  }
  // Pagination runs against the whole sorted/filtered catalog, with independent page controls.
  catalog = [...mods, ...Array.from({length: 19}, (_, n) => ({...mods[2], id: n + 5, name: `Paged mod ${String(n + 5).padStart(2, '0')}`, updated_at: timestamp + n + 5, description: 'Another catalog entry.'}))];
  await call('Emulation.setDeviceMetricsOverride', {width: 1200, height: 850, deviceScaleFactor: 1, mobile: false}, session);
  await call('Page.navigate', {url: 'http://he-mod-index.test/'}, session);
  await until("document.querySelectorAll('[data-index-mod]').length === 10");
  assert.equal(await evaluate("document.querySelector('[data-index-mod]').dataset.indexMod"), '23');
  assert.equal(await evaluate("document.querySelector('[data-pagination] [role=status]').textContent"), 'Page 1 of 3');
  assert(await evaluate("document.querySelector('[data-page-step=\"-1\"]').disabled"));
  await evaluate("document.querySelector('[data-page-step=\"1\"]').click()");
  assert.equal(await evaluate("document.querySelector('[data-index-mod]').dataset.indexMod"), '13');
  assert.equal(await evaluate("document.activeElement.dataset.pageStep"), '1');
  await screenshot('pagination-desktop.png');
  await evaluate("document.querySelector('[data-page-step=\"1\"]').click()");
  assert.equal(await evaluate("document.querySelectorAll('[data-index-mod]').length"), 3);
  assert(await evaluate("[...document.querySelectorAll('[data-page-step=\"1\"]')].every(b => b.disabled)"));
  await evaluate("document.querySelector('[data-page-step=\"-1\"]').click()");
  assert.equal(await evaluate("document.querySelectorAll('[data-index-mod]').length"), 10);
  await evaluate("document.querySelector('[data-sort=name]').click()");
  assert.equal(await evaluate("document.querySelector('[data-pagination] [role=status]').textContent"), 'Page 1 of 3');
  assert.equal(await evaluate("document.querySelector('[data-index-mod]').dataset.indexMod"), '2');
  await evaluate("document.querySelector('[data-page-step=\"1\"]').click()");
  const pageTwo = await evaluate("[...document.querySelectorAll('[data-index-mod]')].map(row => row.dataset.indexMod)");
  await evaluate("document.querySelector('a.mod-name').click()");
  await until("Boolean(document.querySelector('.detail-header'))");
  await evaluate('history.back()');
  await until("Boolean(document.querySelector('[data-pagination]'))");
  assert.deepEqual(await evaluate("[...document.querySelectorAll('[data-index-mod]')].map(row => row.dataset.indexMod)"), pageTwo);
  await evaluate("{const input = document.querySelector('#filter'); input.value = 'unique-search-word'; input.dispatchEvent(new Event('input'));}");
  assert.equal(await evaluate("document.querySelectorAll('[data-index-mod]').length"), 1);
  assert.equal(await evaluate("document.querySelector('[data-index-mod]').dataset.indexMod"), '1');
  assert(await evaluate("[...document.querySelectorAll('[data-pagination]')].every(p => p.hidden)"));
  await evaluate("document.querySelector('#filter').value = ''; document.querySelector('#filter').dispatchEvent(new Event('input'))");
  assert.equal(await evaluate("document.querySelector('[data-pagination] [role=status]').textContent"), 'Page 1 of 3');
  await call('Emulation.setDeviceMetricsOverride', {width: 390, height: 844, deviceScaleFactor: 1, mobile: true}, session);
  assert(await evaluate("document.documentElement.scrollWidth <= innerWidth + 1"), 'Pagination overflows on mobile');
  await screenshot('pagination-mobile.png');
  await evaluate("document.querySelector('[data-page-step=\"1\"]').click(); document.querySelector('[data-page-step=\"1\"]').click()");
  catalog = mods;
  await evaluate('render()');
  await until("document.querySelectorAll('[data-index-mod]').length === 4");
  assert(await evaluate("[...document.querySelectorAll('[data-pagination]')].every(p => p.hidden)"));
  await evaluate("document.querySelector('#filter').value = 'no-matches-in-catalog'; document.querySelector('#filter').dispatchEvent(new Event('input'))");
  assert.equal(await evaluate("document.querySelectorAll('[data-index-mod]').length"), 0);
  assert.equal(await evaluate("document.querySelector('.empty').textContent"), 'No matches.');
  assert.deepEqual(errors, [], `Browser errors: ${errors.join('\n')}`);
  console.log(`Browser checks passed: column order, post styling, equal collapsed row heights (${heights[0]}px desktop), text truncation, Show more/less, sorting, full-text filtering, escaped content, mobile scrolling, local fallback images, optional image input, anonymous navigation, admin import resolution, pagination/search/sort/page retention, image galleries, source captions, viewer navigation, and slideshow cleanup.`);
})().catch(error => {console.error(error); process.exitCode = 1;}).finally(async () => {
  closing = true;
  if (browser.exitCode === null && !browser.signalCode) {
    const exited = new Promise(resolve => browser.once('exit', resolve));
    try {await call('Browser.close');} catch (_) {}
    await Promise.race([exited, new Promise(resolve => setTimeout(resolve, 2000))]);
    if (browser.exitCode === null && !browser.signalCode) {browser.kill('SIGKILL'); await exited;}
  }
  await fs.promises.rm(profile, {recursive: true, force: true, maxRetries: 5, retryDelay: 100});
});
