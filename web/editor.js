'use strict';

class DownloadEditor {
  constructor(root, downloads) {
    this.root = root;
    this.rows = root.querySelector('[data-download-rows]');
    this.addButton = root.querySelector('[data-add-link]');
    this.sequence = 0;
    this.addButton.addEventListener('click', () => this.add());
    for (const download of downloads.length ? downloads : [{}]) this.add(download);
  }
  add(download = {}) {
    if (this.rows.children.length >= 20) return;
    const number = ++this.sequence;
    const row = document.createElement('div');
    row.className = 'download-row';
    row.innerHTML = `<div><label for="download-filename-${number}">Filename</label><input id="download-filename-${number}" data-filename value="${esc(download.filename || '')}" maxlength="180" placeholder="mod.pck" required></div><div><label for="download-url-${number}">URL</label><input id="download-url-${number}" data-url type="url" value="${esc(download.url || '')}" maxlength="2048" required></div><button type="button" class="link-button danger" data-remove-link aria-label="Remove download link ${number}">Remove</button>`;
    const filename = row.querySelector('[data-filename]');
    const url = row.querySelector('[data-url]');
    let inferred = '';
    url.addEventListener('input', () => {
      if (filename.value && filename.value !== inferred) return;
      try {
        const name = decodeURIComponent(new URL(url.value).pathname.split('/').pop());
        if (name && !/[\\/\x00-\x1f\x7f]/.test(name)) filename.value = inferred = name.slice(0, 180);
      } catch { /* Leave the filename to the author while the URL is incomplete. */ }
    });
    row.querySelector('[data-remove-link]').addEventListener('click', () => {row.remove(); this.sync();});
    this.rows.append(row);
    this.sync();
    if (!download.url && this.rows.children.length > 1) filename.focus();
  }
  sync() {
    this.addButton.disabled = this.rows.children.length >= 20;
    for (const button of this.rows.querySelectorAll('[data-remove-link]')) button.disabled = this.rows.children.length === 1;
  }
  values() {
    return [...this.rows.children].map(row => ({filename: row.querySelector('[data-filename]').value.trim(), url: row.querySelector('[data-url]').value.trim()}));
  }
}

class ImageEditor {
  constructor(root, mod) {
    this.root = root;
    this.mod = mod;
    this.image = null;
    this.dirty = false;
    this.removed = false;
    this.existing = false;
    this.generation = 0;
    this.disposed = false;
    this.loadedURL = '';
    this.url = root.querySelector('[data-image-url]');
    this.file = root.querySelector('[data-image-file]');
    this.cropPanel = root.querySelector('[data-crop-panel]');
    this.canvas = root.querySelector('canvas');
    this.zoom = root.querySelector('[data-crop-zoom]');
    this.horizontal = root.querySelector('[data-crop-x]');
    this.vertical = root.querySelector('[data-crop-y]');
    this.removeButton = root.querySelector('[data-remove-image]');
    this.ready = Promise.resolve();
    root.querySelector('[data-load-image]').addEventListener('click', async event => {
      const button = event.currentTarget;
      button.disabled = true; notice('');
      try {this.ready = this.loadRemote(); await this.ready;} catch (error) {notice(error.message);} finally {button.disabled = false;}
    });
    this.file.addEventListener('change', async () => {
      const file = this.file.files[0]; if (!file) return;
      notice('');
      try {
        if (file.size > 4 * 1024 * 1024) throw new Error('Image must be at most 4 MiB.');
        if (!['image/jpeg', 'image/png', 'image/gif'].includes(file.type)) throw new Error('Use a PNG, JPEG, or GIF image.');
        this.ready = this.setSource(file, false); await this.ready;
        this.url.value = ''; this.loadedURL = '';
      } catch (error) {this.file.value = ''; notice(error.message);}
    });
    this.removeButton.addEventListener('click', () => {
      this.generation++; this.image = null; this.removed = true; this.dirty = false;
      this.url.value = ''; this.file.value = ''; this.loadedURL = '';
      this.cropPanel.hidden = true; this.removeButton.hidden = true;
      this.ready = Promise.resolve();
    });
    for (const input of [this.zoom, this.horizontal, this.vertical]) input.addEventListener('input', () => {
      if (!this.image) return;
      const size = Math.max(1, Math.round(Math.min(this.image.naturalWidth, this.image.naturalHeight) / Number(this.zoom.value)));
      this.crop = {x: Math.round((this.image.naturalWidth - size) * Number(this.horizontal.value) / 100), y: Math.round((this.image.naturalHeight - size) * Number(this.vertical.value) / 100), size};
      this.dirty = true; this.sync(); this.draw();
    });
    let pointer = null;
    this.canvas.addEventListener('pointerdown', event => {
      if (!this.image) return;
      event.preventDefault(); this.canvas.focus();
      pointer = {id: event.pointerId, x: event.clientX, y: event.clientY, crop: {...this.crop}};
      this.canvas.setPointerCapture(event.pointerId);
    });
    this.canvas.addEventListener('pointermove', event => {
      if (!pointer || event.pointerId !== pointer.id) return;
      const scale = pointer.crop.size / this.canvas.getBoundingClientRect().width;
      this.move(pointer.crop.x - (event.clientX - pointer.x) * scale, pointer.crop.y - (event.clientY - pointer.y) * scale);
    });
    for (const type of ['pointerup', 'pointercancel', 'lostpointercapture']) this.canvas.addEventListener(type, () => {pointer = null;});
    this.canvas.addEventListener('keydown', event => {
      if (!this.image || !['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown'].includes(event.key)) return;
      event.preventDefault(); const step = Math.max(1, this.crop.size / (event.shiftKey ? 10 : 100));
      this.move(this.crop.x + (event.key === 'ArrowLeft' ? -step : event.key === 'ArrowRight' ? step : 0), this.crop.y + (event.key === 'ArrowUp' ? -step : event.key === 'ArrowDown' ? step : 0));
    });
    if (mod.image_url?.startsWith('/uploads/')) {
      this.ready = this.setSource(mod.image_url, true, mod.image_crop);
      this.ready.catch(error => {if (!this.disposed) notice(error.message);});
    } else if (mod.image_url) {
      this.url.value = mod.image_url;
      this.removeButton.hidden = false;
    }
  }
  async setSource(source, existing, crop) {
    const generation = ++this.generation;
    const image = new Image();
    const objectURL = source instanceof Blob ? URL.createObjectURL(source) : null;
    image.src = objectURL || source;
    try {await image.decode();} catch {throw new Error('Could not read image.');} finally {if (objectURL) URL.revokeObjectURL(objectURL);}
    if (this.disposed || generation !== this.generation) return;
    const width = image.naturalWidth, height = image.naturalHeight;
    if (width > 4096 || height > 4096 || width * height > 12000000) throw new Error('Image exceeds 4096 pixels per side or 12 megapixels.');
    const size = Math.min(width, height);
    this.image = image; this.existing = existing; this.removed = false; this.dirty = !existing;
    this.crop = crop?.size > 0 && crop.x >= 0 && crop.y >= 0 && crop.x + crop.size <= width && crop.y + crop.size <= height ? {...crop} : {x: Math.floor((width - size) / 2), y: Math.floor((height - size) / 2), size};
    this.cropPanel.hidden = false; this.removeButton.hidden = false;
    this.sync(); this.draw();
  }
  async loadRemote() {
    const url = this.url.value.trim(); if (!url) throw new Error('Enter an image URL.');
    const generation = ++this.generation;
    const response = await fetch('/api/images/preview', {method: 'POST', credentials: 'same-origin', headers: {'Content-Type': 'application/json', 'X-CSRF-Token': session.csrf}, body: JSON.stringify({url})});
    if (!response.ok) {const data = await response.json(); throw new Error(data.error || 'Could not load image.');}
    const blob = await response.blob();
    if (this.disposed || generation !== this.generation) return;
    await this.setSource(blob, false); this.loadedURL = url; this.file.value = '';
  }
  move(x, y) {
    this.crop.x = Math.round(Math.max(0, Math.min(this.image.naturalWidth - this.crop.size, x)));
    this.crop.y = Math.round(Math.max(0, Math.min(this.image.naturalHeight - this.crop.size, y)));
    this.dirty = true; this.sync(); this.draw();
  }
  sync() {
    this.zoom.max = String(Math.max(4, Math.min(this.image.naturalWidth, this.image.naturalHeight) / this.crop.size));
    this.zoom.value = String(Math.min(this.image.naturalWidth, this.image.naturalHeight) / this.crop.size);
    for (const [input, span, value] of [[this.horizontal, this.image.naturalWidth - this.crop.size, this.crop.x], [this.vertical, this.image.naturalHeight - this.crop.size, this.crop.y]]) {
      input.disabled = span === 0; input.value = span ? String(value * 100 / span) : '50';
    }
  }
  draw() {
    const context = this.canvas.getContext('2d');
    context.fillStyle = '#fff'; context.fillRect(0, 0, 256, 256);
    context.drawImage(this.image, this.crop.x, this.crop.y, this.crop.size, this.crop.size, 0, 0, 256, 256);
  }
  async prepare() {
    await this.ready;
    if (this.url.value.trim() && this.url.value.trim() !== this.loadedURL) await this.loadRemote();
    if ((this.file.files[0] || this.loadedURL) && !this.image) throw new Error('Load an image before saving.');
  }
  imageURL() {return !this.removed && this.mod.image_url?.startsWith('/uploads/') ? this.mod.image_url : '';}
  async save(id) {
    if (!this.image || !this.dirty || this.removed) return;
    if (this.existing) {await api(`/api/mods/${id}/image/crop`, {method: 'POST', body: this.crop}); return;}
    // Normalize browser-applied orientation and preserve the full screenshot.
    const canvas = document.createElement('canvas');
    canvas.width = this.image.naturalWidth; canvas.height = this.image.naturalHeight;
    const context = canvas.getContext('2d'); context.fillStyle = '#fff'; context.fillRect(0, 0, canvas.width, canvas.height); context.drawImage(this.image, 0, 0);
    let blob;
    for (const quality of [0.9, 0.75, 0.6, 0.45]) {blob = await new Promise(resolve => canvas.toBlob(resolve, 'image/jpeg', quality)); if (blob && blob.size <= 4 * 1024 * 1024) break;}
    if (!blob || blob.size > 4 * 1024 * 1024) throw new Error('Image remains too large after compression.');
    const query = new URLSearchParams(this.crop);
    await api(`/api/mods/${id}/image?${query}`, {method: 'POST', body: blob, headers: {'Content-Type': 'image/jpeg'}});
  }
  dispose() {this.disposed = true; this.generation++; this.image = null;}
}
