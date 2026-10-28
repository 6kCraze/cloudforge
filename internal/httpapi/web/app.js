'use strict';
const $ = (id) => document.getElementById(id);
const statusNames = { queued: 'Queued', running: 'Running', retry_wait: 'Waiting to retry', succeeded: 'Succeeded', failed: 'Failed' };
const components = {
  api: ['ACCEPT / GO API', 'The front door.', 'Validates JPEG and PNG uploads, stores the originals, and creates durable image jobs. An accepted response means your batch was saved.'],
  database: ['PERSIST / POSTGRESQL', 'The city remembers.', 'Jobs and outbox events are saved together. PostgreSQL owns job states, worker claims, leases, retry decisions and the winning result.'],
  dispatcher: ['DISPATCH / OUTBOX', 'A promise, delivered.', 'Reads unpublished outbox events and publishes with broker confirmations. It also recovers expired leases and records fresh delivery intent. Its activity is conceptual here; this API exposes no dispatch telemetry.'],
  broker: ['DELIVER WORK / RABBITMQ', 'The message hub.', 'Delivers job notifications to competing workers with manual acknowledgements. Delivery can happen more than once. Queue depth and message transfers are not exposed in this view.'],
  workers: ['EXECUTE / GO WORKERS', 'Work with an owner.', 'A worker atomically claims a job, renews its lease, and writes an attempt-specific thumbnail. Stale workers cannot commit a result. Buildings represent a worker pool, not a live worker count.'],
  storage: ['DELIVER RESULTS / FILE STORAGE', 'Only the winner ships.', 'Original images and thumbnail candidates live on local disk. Downloads follow the successful attempt recorded in PostgreSQL. Old attempts cannot overwrite the winning file.']
};
let selected = [], uploading = false, current = null, timer = null, generation = 0, pollRequest = null, paused = false;
let snapshotAt = null, acceptedBatchID = null;
const terminal = (batch) => batch.job_count > 0 && batch.succeeded + batch.failed === batch.job_count;
function showError(id, message) { $(id).textContent = message || ''; $(id).hidden = !message; }
function element(tag, className, text) { const node = document.createElement(tag); if (className) node.className = className; if (text !== undefined) node.textContent = text; return node; }
function clock(value) { const date = new Date(value); return Number.isFinite(date.getTime()) ? date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : 'Unavailable'; }
function selectComponent(id) {
  const info = components[id]; if (!info) return;
  $('component-kind').textContent = info[0]; $('component-title').textContent = info[1]; $('component-description').textContent = info[2];
  document.querySelectorAll('[data-component]').forEach((button) => button.setAttribute('aria-pressed', String(button.dataset.component === id)));
  document.querySelectorAll('[data-building]').forEach((building) => building.classList.toggle('is-selected', building.dataset.building === id));
}
document.querySelectorAll('[data-component]').forEach((button) => button.addEventListener('click', () => selectComponent(button.dataset.component)));
document.querySelectorAll('[data-building]').forEach((building) => building.addEventListener('click', () => selectComponent(building.dataset.building)));
selectComponent('api');

// These highlights describe the selected batch snapshot, never broker traffic or service health.
function updateScene(batch, freshness = 'saved') {
  document.querySelectorAll('[data-building]').forEach((building) => building.removeAttribute('data-observed'));
  if (!batch?.jobs) {
    $('scene-status').textContent = 'ARCHITECTURE VIEW';
    $('scene-observation').textContent = 'Upload or open a batch to connect this map to saved job states. Routes are illustrative, never live message telemetry.';
    return;
  }
  const mark = (id, state) => document.querySelector('[data-building="' + id + '"]').setAttribute('data-observed', state);
  mark('database', batch.failed ? 'failed' : batch.retry_wait ? 'retry_wait' : batch.queued ? 'queued' : batch.running ? 'running' : 'succeeded');
  if (batch.running) mark('workers', 'running');
  if (batch.retry_wait) mark('workers', 'retry_wait');
  if (batch.failed && !batch.running && !batch.retry_wait) mark('workers', 'failed');
  if (batch.succeeded) mark('storage', 'succeeded');
  if (batch.id === acceptedBatchID) mark('api', 'succeeded');
  const states = [['queued', batch.queued], ['running', batch.running], ['waiting to retry', batch.retry_wait || 0], ['succeeded', batch.succeeded], ['failed', batch.failed]].filter(([, count]) => count > 0).map(([name, count]) => count + ' ' + name).join(' · ');
  $('scene-status').textContent = freshness === 'saved' ? 'SAVED BATCH / ' + clock(snapshotAt) : 'LAST SAVED SNAPSHOT / UPDATES PAUSED';
  $('scene-observation').textContent = states + '. PostgreSQL job states only. Routes, dispatcher and broker activity remain illustrative.';
}
function updateSelection() {
  $('selected-files').replaceChildren();
  selected.forEach((item, index) => {
    const row = element('li'), preview = element('img', 'file-preview'); preview.src = item.url; preview.alt = 'Selected image ' + (index + 1);
    const info = element('div', 'file-info'); info.append(element('span', 'file-name', item.file.name), element('small', '', (item.file.size / 1024).toFixed(1) + ' KiB · ' + (item.file.type || 'Image')));
    const remove = element('button', 'remove-file', '×'); remove.type = 'button'; remove.setAttribute('aria-label', 'Remove ' + item.file.name); remove.disabled = uploading;
    remove.addEventListener('click', () => { URL.revokeObjectURL(item.url); selected.splice(index, 1); showError('selection-error', ''); updateSelection(); });
    row.append(preview, info, remove); $('selected-files').append(row);
  });
  $('selection-count').textContent = selected.length + ' / 4 selected';
  $('process').disabled = uploading || selected.length === 0;
  $('process').replaceChildren(document.createTextNode(uploading ? 'Uploading images…' : selected.length ? 'Process ' + selected.length + ' image' + (selected.length === 1 ? '' : 's') : 'Process images'), element('span', '', '↗'));
  $('choose').disabled = uploading;
}
function addFiles(files) {
  if (uploading) return;
  const incoming = Array.from(files);
  if (selected.length + incoming.length > 4) { showError('selection-error', 'Choose up to four images. Remove a selected image to make room.'); return; }
  for (const file of incoming) {
    if (file.size === 0 || file.size > 5 * 1024 * 1024) { showError('selection-error', file.name + ': images must contain data and be at most 5 MiB.'); return; }
    if (!['image/jpeg', 'image/png'].includes(file.type) && !(file.type === '' && /\.(jpe?g|png)$/i.test(file.name))) { showError('selection-error', file.name + ': choose a JPEG or PNG image.'); return; }
  }
  incoming.forEach((file) => selected.push({ file, url: URL.createObjectURL(file) }));
  showError('selection-error', ''); updateSelection();
}
$('choose').addEventListener('click', () => $('files').click());
$('files').addEventListener('change', () => { addFiles($('files').files); $('files').value = ''; });
['dragenter', 'dragover'].forEach((event) => $('drop-zone').addEventListener(event, (e) => { e.preventDefault(); if (!uploading) $('drop-zone').classList.add('drag-over'); }));
['dragleave', 'drop'].forEach((event) => $('drop-zone').addEventListener(event, (e) => { e.preventDefault(); $('drop-zone').classList.remove('drag-over'); if (event === 'drop') addFiles(e.dataTransfer.files); }));
window.addEventListener('dragover', (e) => e.preventDefault());
window.addEventListener('drop', (e) => e.preventDefault());

async function apiResponse(response) {
  let data; try { data = await response.json(); } catch { throw new Error('The server returned an unreadable response.'); }
  if (!response.ok) {
    const error = new Error(data.error?.message || 'Request failed (' + response.status + ').');
    error.status = response.status; throw error;
  }
  return data;
}
function stopPolling() { generation++; clearTimeout(timer); timer = null; if (pollRequest) pollRequest.abort(); pollRequest = null; }
function failureReason(job) {
  if (job.error_code === 'lease_expired') return 'The worker lease expired before completion.';
  if (job.error_code === 'invalid_image') return 'The image could not be decoded within the supported format and size limits.';
  if (job.error_code === 'invalid_job') return 'The required image or job input is missing or invalid.';
  if (job.error_code === 'execution_failed') return job.status === 'failed' ? 'Processing could not finish within the allowed attempts.' : 'This attempt could not finish. A retry was saved.';
  return 'Processing could not finish. Inspect the server logs with this job ID for details.';
}
function appendJobFacts(detail, job) {
  const details = element('details', 'job-details'), summary = element('summary', '', 'Latest attempt details');
  const facts = element('dl', 'job-facts');
  const fact = (name, value) => facts.append(element('dt', '', name), element('dd', '', value));
  fact('Attempts', String(job.attempt_count));
  fact(job.status === 'running' ? 'Worker' : job.claimed_by ? 'Last worker' : 'Current owner', job.claimed_by || 'Unassigned');
  if (job.started_at) fact('Started', clock(job.started_at));
  if (job.completed_at && job.started_at) {
    const duration = new Date(job.completed_at) - new Date(job.started_at);
    if (Number.isFinite(duration) && duration >= 0) fact('Last duration', duration < 1000 ? duration + ' ms' : (duration / 1000).toFixed(2) + ' s');
  }
  if (job.next_retry_at) fact('Retry due', clock(job.next_retry_at));
  if (job.attempt_id) fact('Attempt ID', job.attempt_id);
  details.append(summary, facts); detail.append(details);
}
function displayBatch(batch) {
  $('retry-load').hidden = true;
  current = batch; snapshotAt = new Date(); $('empty-state').hidden = true; $('batch-view').hidden = false;
  $('batch-id').textContent = 'Batch ' + batch.id; $('batch-link').href = '#batch=' + batch.id;
  const done = batch.succeeded + batch.failed;
  $('progress-text').textContent = done + ' of ' + batch.job_count + ' finished';
  $('progress').max = batch.job_count; $('progress').value = done;
  $('counts').replaceChildren(...[['Queued', batch.queued], ['Running', batch.running], ['Retrying', batch.retry_wait || 0], ['Succeeded', batch.succeeded], ['Failed', batch.failed]].map(([name, count]) => { const node = element('span', '', name); node.append(element('b', '', count)); return node; }));
  $('batch-state').textContent = terminal(batch) ? batch.failed ? 'Finished with errors' : 'Batch complete' : batch.retry_wait ? 'Recovery in progress' : 'In progress';
  $('batch-message').textContent = terminal(batch) ? batch.failed ? 'All jobs have finished. Completed thumbnails are available; review failed jobs below.' : 'Success is recorded. Your thumbnails are ready to preview and download.' : batch.retry_wait ? 'A failed attempt is waiting for its saved retry time. Recovery runs independently of this page.' : batch.running ? 'A worker owns an active attempt. This view follows saved job states.' : 'Jobs are saved and queued. This does not confirm broker delivery or worker availability.';
  $('snapshot-time').textContent = 'Saved state retrieved ' + clock(snapshotAt) + (terminal(batch) ? ' · Final snapshot' : ' · Refreshes every 2 s');
  $('poll-control').hidden = terminal(batch); $('poll-control').textContent = paused ? 'Resume updates' : 'Pause updates';
  updateScene(batch);
  const signature = JSON.stringify(batch.jobs);
  if ($('results').dataset.signature === signature) return;
  const expanded = new Set(Array.from($('results').querySelectorAll('details[open]')).map((item) => item.closest('[data-job-id]').dataset.jobId));
  const focusedCard = document.activeElement?.closest('[data-job-id]');
  const focusedJob = focusedCard?.dataset.jobId;
  const focusedControl = document.activeElement?.tagName === 'SUMMARY' ? 'summary' : document.activeElement?.classList.contains('download') ? '.download' : null;
  $('results').dataset.signature = signature; $('results').replaceChildren();
  batch.jobs.forEach((job) => {
    const card = element('article', 'result'); card.dataset.jobId = job.id;
    const visual = element('div', 'thumbnail-area' + (job.status === 'succeeded' ? '' : ' job-placeholder'));
    const detail = element('div', 'result-detail'); detail.append(element('span', 'result-name', job.original_name), element('span', 'state ' + job.status, statusNames[job.status] || 'Unknown state'));
    if (job.status === 'succeeded' && job.output_url) {
      const image = element('img'); image.src = job.output_url; image.alt = 'Thumbnail of ' + job.original_name; image.loading = 'lazy';
      image.addEventListener('error', () => { visual.replaceChildren(element('span', '', '!')); detail.append(element('p', 'failure', 'Success was recorded, but this output could not be loaded. Its file may be missing or incomplete.')); }, { once: true }); visual.append(image);
      detail.append(element('div', 'result-dimensions', job.result?.width + ' × ' + job.result?.height + ' px · PNG'));
      const link = element('a', 'download', 'Download PNG'); link.href = job.output_url; link.download = 'thumbnail-' + job.id + '.png'; link.setAttribute('aria-label', 'Download thumbnail of ' + job.original_name); link.append(element('span', '', '↓')); detail.append(link);
    } else {
      visual.append(element('span', '', job.status === 'failed' ? '!' : job.status === 'retry_wait' ? '↻' : '◇'));
      if (job.status === 'failed') detail.append(element('p', 'failure', failureReason(job)));
      if (job.status === 'retry_wait') detail.append(element('p', 'retry-note', failureReason(job) + (job.next_retry_at ? ' Retry due ' + clock(job.next_retry_at) + '.' : '')));
    }
    appendJobFacts(detail, job);
    if (expanded.has(job.id)) detail.querySelector('details').open = true;
    detail.append(element('span', 'job-id', 'Job ' + job.id)); card.append(visual, detail); $('results').append(card);
    if (job.id === focusedJob && focusedControl) card.querySelector(focusedControl)?.focus({ preventScroll: true });
  });
}
function pauseSnapshot() {
  if (current?.jobs) {
    updateScene(current, 'paused');
    $('snapshot-time').textContent = 'Last saved state retrieved ' + clock(snapshotAt) + ' · Updates paused';
  }
}
async function refreshBatch(id, epoch) {
  if (epoch !== generation || paused) return;
  const controller = new AbortController(); pollRequest = controller;
  const timeout = setTimeout(() => controller.abort(), 10000);
  try {
    const batch = await apiResponse(await fetch('/v1/batches/' + id, { cache: 'no-store', signal: controller.signal }));
    if (epoch !== generation) return;
    showError('api-error', ''); displayBatch(batch);
    if (!terminal(batch)) timer = setTimeout(() => refreshBatch(id, epoch), 2000);
  } catch (error) {
    if (epoch !== generation) return;
    paused = true; $('poll-control').hidden = false; $('poll-control').textContent = 'Resume updates'; pauseSnapshot();
    $('retry-load').hidden = !!current?.jobs;
    if (!current?.jobs) $('batch-state').textContent = 'Batch unavailable';
    showError('api-error', error.status === 404 ? 'This batch was not found. Check the saved link or upload new images.' : 'Updates paused: ' + (error.name === 'AbortError' ? 'the request timed out' : error.message) + '. Resume when the API is available. No automatic resubmission will occur.');
  } finally { clearTimeout(timeout); if (pollRequest === controller) pollRequest = null; }
}
function loadBatch(id) {
  stopPolling(); paused = false; current = { id }; acceptedBatchID = null; snapshotAt = null;
  $('results').replaceChildren(); delete $('results').dataset.signature; $('batch-view').hidden = true; $('empty-state').hidden = false; $('batch-state').textContent = 'Loading batch';
  updateScene(null); $('retry-load').hidden = true; showError('api-error', ''); refreshBatch(id, generation);
}
$('retry-load').addEventListener('click', () => {
  if (current?.id) { paused = false; $('retry-load').hidden = true; refreshBatch(current.id, generation); }
});
function batchFromHash() { return /^#batch=([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/i.exec(location.hash)?.[1]; }
$('poll-control').addEventListener('click', () => {
  if (!current) return;
  if (paused) { paused = false; showError('api-error', ''); $('poll-control').textContent = 'Pause updates'; refreshBatch(current.id, generation); }
  else { stopPolling(); paused = true; $('poll-control').textContent = 'Resume updates'; $('batch-message').textContent = 'Updates paused. Jobs continue independently; resume to retrieve saved states.'; pauseSnapshot(); }
});
$('process').addEventListener('click', async () => {
  if (uploading || selected.length === 0) return;
  uploading = true; updateSelection(); showError('api-error', ''); stopPolling();
  const epoch = generation, body = new FormData(); selected.forEach((item) => body.append('images', item.file, item.file.name));
  const controller = new AbortController(), timeout = setTimeout(() => controller.abort(), 60000);
  try {
    const batch = await apiResponse(await fetch('/v1/batches', { method: 'POST', body, signal: controller.signal }));
    if (epoch !== generation) return;
    paused = false; acceptedBatchID = batch.id; history.replaceState(null, '', '#batch=' + batch.id); displayBatch(batch);
    selected.forEach((item) => URL.revokeObjectURL(item.url)); selected = [];
    if (!terminal(batch)) timer = setTimeout(() => refreshBatch(batch.id, generation), 2000);
  } catch (error) {
    if (epoch !== generation) return;
    showError('api-error', error.name === 'AbortError' ? 'Upload response timed out. It may have been saved; check the API logs before submitting again.' : error.message);
    if (current && !terminal(current)) { paused = true; $('poll-control').hidden = false; $('poll-control').textContent = 'Resume updates'; pauseSnapshot(); }
  } finally { clearTimeout(timeout); uploading = false; updateSelection(); }
});
window.addEventListener('hashchange', () => {
  const id = batchFromHash();
  if (id && current?.id !== id) loadBatch(id);
});
window.addEventListener('pagehide', (event) => {
  stopPolling();
  // A bfcache page can return with its selected files and DOM intact.
  if (!event.persisted) selected.forEach((item) => URL.revokeObjectURL(item.url));
});
window.addEventListener('pageshow', (event) => {
  if (event.persisted && current?.jobs && !terminal(current) && !paused) refreshBatch(current.id, generation);
});

// One scheduled frame per scroll event. No render loop; CSS route motion pauses offscreen.
const scene = $('city-scene'), reducedMotion = matchMedia('(prefers-reduced-motion: reduce)');
let userReducedMotion = false;
const motionReduced = () => reducedMotion.matches || userReducedMotion;
function syncMotion() {
  document.documentElement.classList.toggle('motion-reduced', motionReduced());
  $('motion-control').setAttribute('aria-pressed', String(motionReduced()));
  $('motion-control').textContent = reducedMotion.matches ? 'System: reduced motion' : userReducedMotion ? 'Motion reduced' : 'Reduce motion';
  $('motion-control').disabled = reducedMotion.matches;
  scene.style.removeProperty('--journey'); document.querySelector('.hero-copy').style.removeProperty('--hero-scroll'); scheduleJourney();
}
$('motion-control').addEventListener('click', () => { userReducedMotion = !userReducedMotion; syncMotion(); });
let sceneVisible = false, scrollFrame = null;
function updateJourney() {
  scrollFrame = null;
  if (!sceneVisible || motionReduced() || innerWidth <= 800) { scene.style.removeProperty('--journey'); return; }
  const rect = document.querySelector('.journey').getBoundingClientRect();
  const progress = Math.max(0, Math.min(1, -rect.top / Math.max(1, rect.height - innerHeight)));
  scene.style.setProperty('--journey', progress.toFixed(3));
  document.querySelector('.hero-copy').style.setProperty('--hero-scroll', Math.min(1, progress * 3).toFixed(3));
  if (progress < .1) delete scene.dataset.chapter;
}
function scheduleJourney() { if (scrollFrame === null && sceneVisible && !motionReduced()) scrollFrame = requestAnimationFrame(updateJourney); }
if ('IntersectionObserver' in window) {
  new IntersectionObserver(([entry]) => { sceneVisible = entry.isIntersecting; scene.classList.toggle('is-visible', sceneVisible); scheduleJourney(); }, { threshold: 0 }).observe(scene);
  const chapterObserver = new IntersectionObserver((entries) => entries.forEach((entry) => {
    entry.target.classList.toggle('chapter-active', entry.isIntersecting);
    if (entry.isIntersecting) scene.dataset.chapter = entry.target.dataset.chapter;
  }), { threshold: .5 });
  document.querySelectorAll('[data-chapter]').forEach((chapter) => chapterObserver.observe(chapter));
} else {
  document.querySelectorAll('[data-chapter]').forEach((chapter) => chapter.classList.add('chapter-active'));
}
addEventListener('scroll', scheduleJourney, { passive: true });
addEventListener('resize', scheduleJourney, { passive: true });
reducedMotion.addEventListener('change', syncMotion);
syncMotion();
const initialBatch = batchFromHash();
if (initialBatch) { loadBatch(initialBatch); $('workspace').scrollIntoView({ behavior: 'instant' }); }
updateSelection();
