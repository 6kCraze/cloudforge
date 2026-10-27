'use strict';
const $ = (id) => document.getElementById(id);
let selected = [], uploading = false, current = null, timer = null, generation = 0, pollRequest = null, paused = false;
const terminal = (batch) => batch.job_count > 0 && batch.succeeded + batch.failed === batch.job_count;
function showError(id, message) { $(id).textContent = message || ''; $(id).hidden = !message; }
function element(tag, className, text) { const node = document.createElement(tag); if (className) node.className = className; if (text !== undefined) node.textContent = text; return node; }
function updateSelection() {
  $('selected-files').replaceChildren();
  selected.forEach((item, index) => {
    const row = element('li'), preview = element('img', 'file-preview'); preview.src = item.url; preview.alt = `Selected image ${index + 1}`;
    const info = element('div', 'file-info'); info.append(element('span', 'file-name', item.file.name), element('small', '', `${(item.file.size / 1024).toFixed(1)} KiB · ${item.file.type || 'Image'}`));
    const remove = element('button', 'remove-file', '×'); remove.type = 'button'; remove.setAttribute('aria-label', `Remove ${item.file.name}`); remove.disabled = uploading;
    remove.addEventListener('click', () => { URL.revokeObjectURL(item.url); selected.splice(index, 1); showError('selection-error', ''); updateSelection(); });
    row.append(preview, info, remove); $('selected-files').append(row);
  });
  $('selection-count').textContent = `${selected.length} / 4 selected`;
  $('process').disabled = uploading || selected.length === 0;
  $('process').replaceChildren(document.createTextNode(uploading ? 'Uploading your images…' : `Process ${selected.length || ''} image${selected.length === 1 ? '' : 's'}`), element('span', '', '→'));
  $('choose').disabled = uploading;
}
function addFiles(files) {
  if (uploading) return;
  const incoming = Array.from(files);
  if (selected.length + incoming.length > 4) { showError('selection-error', 'Choose up to four images. Remove a selected image to make room.'); return; }
  for (const file of incoming) {
    if (file.size === 0 || file.size > 5 * 1024 * 1024) { showError('selection-error', `${file.name}: images must contain data and be at most 5 MiB.`); return; }
    if (!['image/jpeg', 'image/png'].includes(file.type) && !(file.type === '' && /\.(jpe?g|png)$/i.test(file.name))) { showError('selection-error', `${file.name}: choose a JPEG or PNG image.`); return; }
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
  if (!response.ok) throw new Error(data.error?.message || `Request failed (${response.status}).`);
  return data;
}
function stopPolling() { generation++; clearTimeout(timer); timer = null; if (pollRequest) pollRequest.abort(); pollRequest = null; }
function displayBatch(batch) {
  current = batch; $('empty-state').hidden = true; $('batch-view').hidden = false;
  $('batch-id').textContent = `Batch ${batch.id}`; $('batch-link').href = `#batch=${batch.id}`;
  const done = batch.succeeded + batch.failed;
  $('progress-text').textContent = `${done} of ${batch.job_count} finished`;
  $('progress').max = batch.job_count; $('progress').value = done;
	$('counts').replaceChildren(...[['Queued', batch.queued], ['Running', batch.running], ['Waiting to retry', batch.retry_wait || 0], ['Succeeded', batch.succeeded], ['Failed', batch.failed]].map(([name, count]) => element('span', '', `${name} ${count}`)));
  $('batch-state').textContent = terminal(batch) ? (batch.failed ? 'Finished with errors' : 'Batch complete') : 'Processing';
  $('batch-message').textContent = terminal(batch) ? (batch.failed ? 'Finished images are ready. Review the failure details below.' : 'Your thumbnails are ready to preview and download.') : (batch.retry_wait ? 'A job is waiting for its next attempt. Updates reflect saved job states.' : batch.running ? 'Your worker is processing images. Updates reflect saved job states.' : 'Images are queued. Processing will begin when a worker is available.');
  $('poll-control').hidden = terminal(batch); $('poll-control').textContent = paused ? 'Resume updates' : 'Pause updates';
  // Preserve existing preview nodes when a polling response has no job changes.
  const signature = JSON.stringify(batch.jobs);
  if ($('results').dataset.signature === signature) return;
  $('results').dataset.signature = signature; $('results').replaceChildren();
  batch.jobs.forEach((job) => {
    const card = element('article', 'result'); card.dataset.jobId = job.id;
    const visual = element('div', `thumbnail-area${job.status === 'succeeded' ? '' : ' job-placeholder'}`);
    const detail = element('div', 'result-detail'); detail.append(element('span', 'result-name', job.original_name), element('span', `state ${job.status}`, job.status === 'retry_wait' ? 'Waiting to retry' : job.status));
    if (job.status === 'succeeded' && job.output_url) {
      const image = element('img'); image.src = job.output_url; image.alt = `Thumbnail of ${job.original_name}`;
      image.addEventListener('error', () => { visual.replaceChildren(element('span', '', 'Unavailable')); detail.append(element('p', 'failure', 'The thumbnail could not be loaded. Its output may be missing or incomplete.')); }, { once: true }); visual.append(image);
      detail.append(element('div', 'result-dimensions', `${job.result?.width} × ${job.result?.height} px · PNG`));
      const link = element('a', 'download', 'Download PNG ↓'); link.href = job.output_url; link.download = `thumbnail-${job.id}.png`; detail.append(link);
    } else {
      visual.append(element('span', '', job.status === 'failed' ? '!' : '▧'));
      if (job.status === 'failed') detail.append(element('p', 'failure', job.error_message || job.error_code || 'This image could not be processed.'));
    }
    detail.append(element('span', 'job-id', `Job ${job.id}`)); card.append(visual, detail); $('results').append(card);
  });
}
async function refreshBatch(id, epoch) {
  if (epoch !== generation || paused) return;
  const controller = new AbortController(); pollRequest = controller;
  const timeout = setTimeout(() => controller.abort(), 10000);
  try {
    const batch = await apiResponse(await fetch(`/v1/batches/${id}`, { cache: 'no-store', signal: controller.signal }));
    if (epoch !== generation) return;
    showError('api-error', ''); displayBatch(batch);
    if (!terminal(batch)) timer = setTimeout(() => refreshBatch(id, epoch), 2000);
  } catch (error) {
    if (epoch !== generation) return;
    paused = true; $('batch-view').hidden = false; $('empty-state').hidden = true; $('poll-control').hidden = false; $('poll-control').textContent = 'Resume updates';
    showError('api-error', `Updates paused: ${error.name === 'AbortError' ? 'the request timed out' : error.message} Resume updates when the API is available. No automatic resubmission will occur.`);
  } finally { clearTimeout(timeout); if (pollRequest === controller) pollRequest = null; }
}
function loadBatch(id) { stopPolling(); paused = false; current = { id }; showError('api-error', ''); refreshBatch(id, generation); }
function batchFromHash() { const match = /^#batch=([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/i.exec(location.hash); return match?.[1]; }
$('poll-control').addEventListener('click', () => {
  if (!current) return;
  if (paused) { paused = false; showError('api-error', ''); $('poll-control').textContent = 'Pause updates'; refreshBatch(current.id, generation); }
  else { stopPolling(); paused = true; $('poll-control').textContent = 'Resume updates'; $('batch-message').textContent = 'Updates paused. Your jobs continue independently; resume to check their saved states.'; }
});
$('process').addEventListener('click', async () => {
  if (uploading || selected.length === 0) return;
  uploading = true; updateSelection(); showError('api-error', ''); stopPolling();
  const body = new FormData(); selected.forEach((item) => body.append('images', item.file, item.file.name));
  const controller = new AbortController(), timeout = setTimeout(() => controller.abort(), 60000);
  try {
    const batch = await apiResponse(await fetch('/v1/batches', { method: 'POST', body, signal: controller.signal }));
    paused = false; history.replaceState(null, '', `#batch=${batch.id}`); displayBatch(batch);
    selected.forEach((item) => URL.revokeObjectURL(item.url)); selected = [];
    if (!terminal(batch)) timer = setTimeout(() => refreshBatch(batch.id, generation), 2000);
  } catch (error) {
    showError('api-error', error.name === 'AbortError' ? 'Upload response timed out. It may have been saved; check the API logs before submitting again.' : error.message);
    // A previously displayed batch can still be resumed after a rejected upload.
    if (current && !terminal(current)) { paused = true; $('poll-control').hidden = false; $('poll-control').textContent = 'Resume updates'; }
  } finally { clearTimeout(timeout); uploading = false; updateSelection(); }
});
window.addEventListener('hashchange', () => { const id = batchFromHash(); if (id) loadBatch(id); });
window.addEventListener('pagehide', () => { stopPolling(); selected.forEach((item) => URL.revokeObjectURL(item.url)); });
const initialBatch = batchFromHash(); if (initialBatch) loadBatch(initialBatch); updateSelection();
