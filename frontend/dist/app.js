'use strict';

const q = (selector) => document.querySelector(selector);
const qa = (selector) => [...document.querySelectorAll(selector)];
const api = () => window.go && window.go.main && window.go.main.App;
const state = { page: 'home', format: 'video', jobs: [], settings: null, tools: null, source: null, mediaFile: '' };
const icons = {video:'icon-play',audio:'icon-music',file:'icon-file',convert:'icon-convert'};
const translations = {queued:'Na fila',downloading:'Baixando',converting:'Processando',completed:'Concluído',failed:'Falhou',cancelled:'Cancelado',interrupted:'Interrompido'};
let toastTimer;

function toast(msg) {
  const el = q('#toast'); el.textContent = msg; el.classList.add('show');
  clearTimeout(toastTimer); toastTimer = setTimeout(() => el.classList.remove('show'), 3900);
}
function errorText(err) { return err?.message || String(err || 'Erro inesperado'); }
async function call(name, ...args) {
  const backend = api();
  if (!backend || typeof backend[name] !== 'function') throw new Error('Abra o aplicativo OpenFetch no Windows para usar esta função.');
  return backend[name](...args);
}
function escapeHTML(v) { return String(v ?? '').replace(/[&<>"']/g, (c) => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }
function icon(id) { return `<svg class="icon"><use href="#${id}"></use></svg>`; }
function switchPage(page) {
  if (!['home', 'downloads', 'converter', 'settings'].includes(page)) return;
  state.page = page;
  qa('.page').forEach(el => el.classList.toggle('hidden', el.id !== `page-${page}`));
  qa('.nav-item').forEach(el => el.classList.toggle('active', el.dataset.page === page));
  q('#page-name').textContent = ({home:'Visão geral',downloads:'Downloads',converter:'Conversor',settings:'Configurações'})[page];
}
function chooseFormat(format) {
  state.format = format;
  qa('.format-card').forEach(el => el.classList.toggle('selected', el.dataset.format === format));
  const select = q('#quality-select'); select.disabled = format !== 'video'; select.style.opacity = select.disabled ? '.5' : '1';
}
function fmtBytes(v) {
  if (!Number.isFinite(v) || v < 0) return '—';
  if (v < 1024) return `${v} B`;
  const units=['KB','MB','GB','TB']; let ix=-1;
  do { v /= 1024; ix++; } while (v >= 1024 && ix < 3);
  return `${v.toFixed(1)} ${units[ix]}`;
}
function fmtDuration(n) { const s = Number(n); return !s ? '—' : `${Math.floor(s/60)}:${String(Math.floor(s%60)).padStart(2,'0')}`; }
async function analyze() {
  const url = q('#link-input').value.trim(); if (!url) return toast('Cole um link para começar.');
  const btn = q('#analyze-btn'); btn.disabled = true; q('#analyze-text').textContent = 'Analisando...';
  try {
    const info = await call('Probe', url);
    state.source = info;
    q('#probe-box').classList.remove('hidden');
    q('#probe-title').textContent = info.title || 'Arquivo identificado';
    q('#probe-desc').textContent = `${info.host || 'Origem'} · ${info.kind === 'video' ? `Vídeo · ${fmtDuration(info.duration)}` : `Arquivo · ${fmtBytes(info.size)}`}`;
    const thumb = q('#probe-thumb'); thumb.classList.add('hidden');
    const iconEl = q('#probe-icon'); iconEl.classList.remove('hidden');
    if (info.thumbnail && /^https?:\/\//i.test(info.thumbnail)) {
      thumb.src = info.thumbnail; thumb.classList.remove('hidden'); iconEl.classList.add('hidden');
      thumb.onerror = () => { thumb.classList.add('hidden'); iconEl.classList.remove('hidden'); };
    }
    q('#probe-icon').innerHTML = icon(info.kind === 'video' ? 'icon-play' : 'icon-file');
    chooseFormat(info.kind === 'video' ? 'video' : 'file');
    toast('Link identificado. Escolha como deseja baixar.');
  } catch (err) { state.source = null; q('#probe-box').classList.add('hidden'); toast(errorText(err)); }
  finally { btn.disabled = false; q('#analyze-text').textContent = 'Analisar link'; }
}
async function startDownload() {
  const url = q('#link-input').value.trim(); if (!url) return toast('Cole um link antes de iniciar.');
  const btn = q('#start-btn'); btn.disabled = true;
  try {
    await call('StartDownload', url, state.format, q('#quality-select').value);
    toast('Download adicionado à fila!'); switchPage('downloads'); await refresh();
  } catch(err) { toast(errorText(err)); }
  finally { btn.disabled = false; }
}
function jobMarkup(j) {
  const running = ['queued','downloading','converting'].includes(j.status);
  const pct = Math.min(100, Math.max(0, Number(j.progress) || 0));
  const file = j.filePath ? j.filePath : j.error ? j.error : j.detail || '';
  const subtitle = j.bytesTotal > 0 ? `${fmtBytes(j.bytesDone)} / ${fmtBytes(j.bytesTotal)}` : file;
  return `<article class="job-row">
    <div class="job-icon">${icon(icons[j.kind] || 'icon-file')}</div>
    <div class="job-data"><div class="job-title" title="${escapeHTML(j.title)}">${escapeHTML(j.title)}</div><div class="job-sub" title="${escapeHTML(file)}"><span>${escapeHTML(j.format || '')}</span><span>•</span><span style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${escapeHTML(subtitle)}</span></div></div>
    ${running && j.status !== 'queued' ? `<div class="progress-track" title="${Math.round(pct)}%"><span style="width:${pct}%"></span></div>` : ''}
    <span class="job-status ${escapeHTML(j.status)}">${escapeHTML(translations[j.status] || j.status)} ${running && j.status !== 'queued' && pct > 0 ? Math.round(pct) + '%' : ''}</span>
    ${running ? `<button class="job-cancel" data-cancel="${escapeHTML(j.id)}" title="Cancelar">${icon('icon-close')}</button>` : ''}
  </article>`;
}
function renderJobs() {
  const count = state.jobs.length;
  const done = state.jobs.filter(j=>j.status==='completed').length;
  const active = state.jobs.filter(j=>['queued','downloading','converting'].includes(j.status)).length;
  q('#stat-all').textContent = count; q('#stat-done').textContent = done; q('#stat-active').textContent = active; q('#nav-count').textContent = active;
  const empty = `<div class="empty-state"><div class="empty-circle">${icon('icon-download')}</div><strong>Nada por aqui ainda</strong>Seu primeiro download vai aparecer aqui.</div>`;
  q('#recent-list').innerHTML = state.jobs.length ? state.jobs.slice(0,3).map(jobMarkup).join('') : empty;
  q('#all-list').innerHTML = state.jobs.length ? state.jobs.map(jobMarkup).join('') : empty;
}
function renderState(s) {
  state.jobs = Array.isArray(s.jobs) ? s.jobs : [];
  state.settings = s.settings || null;
  state.tools = s.tools || null;
  if (state.settings) q('#folder-path').textContent = state.settings.downloadDir || 'Não definida';
  if (state.tools) {
    const t = state.tools;
    const tdesc = `${t.ytDlp ? '✓' : '✕'} yt-dlp     •     ${t.ffmpeg ? '✓' : '✕'} FFmpeg     •     ${t.deno ? '✓' : '✕'} Deno`;
    q('#tools-description').textContent = tdesc;
    q('#tools-status').textContent = t.ytDlp && t.ffmpeg && t.deno ? 'Prontos' : 'Incompleto';
    q('#runtime-label').textContent = t.ytDlp && t.ffmpeg && t.deno ? 'Componentes prontos' : 'Componentes incompletos';
  }
  renderJobs();
}
async function refresh() {
  if (!api()) return;
  try { renderState(await call('GetState')); }
  catch (err) { console.error('Erro ao atualizar estado:', err); }
}
async function folder() { try { await call('OpenDownloadDir'); } catch (err) { toast(errorText(err)); } }
async function chooseFolder() {
  try { const dir = await call('ChooseDownloadDir'); if(dir) { toast('Pasta de downloads atualizada.'); await refresh(); } }
  catch (err) { toast(errorText(err)); }
}
async function chooseMedia() {
  try {
    const file = await call('ChooseMediaFile'); if (!file) return;
    state.mediaFile = file; q('#chosen-file').textContent = file;
  } catch(err) { toast(errorText(err)); }
}
async function convertMedia() {
  if (!state.mediaFile) return toast('Selecione um arquivo antes de converter.');
  const btn=q('#convert-start'); btn.disabled = true;
  try { await call('ConvertMedia', state.mediaFile, q('#convert-format').value); toast('Conversão iniciada.'); switchPage('downloads'); await refresh(); }
  catch(err) { toast(errorText(err)); }
  finally { btn.disabled = false; }
}
async function cancelJob(id) { try { await call('CancelDownload', id); toast('Solicitação de cancelamento enviada.'); } catch(err) { toast(errorText(err)); } }

function init() {
  qa('.nav-item').forEach(b => b.addEventListener('click', () => switchPage(b.dataset.page)));
  qa('[data-navigate]').forEach(b => b.addEventListener('click', () => switchPage(b.dataset.navigate)));
  qa('.format-card').forEach(b => b.addEventListener('click', () => chooseFormat(b.dataset.format)));
  q('#analyze-btn').addEventListener('click', analyze);
  q('#start-btn').addEventListener('click', startDownload);
  q('#link-input').addEventListener('keydown', (event) => { if (event.key === 'Enter') analyze(); });
  q('#link-input').addEventListener('input', () => { state.source = null; q('#probe-box').classList.add('hidden'); });
  q('#top-folder').addEventListener('click', folder);
  q('#downloads-folder').addEventListener('click', folder);
  q('#change-folder').addEventListener('click', chooseFolder);
  q('#choose-media').addEventListener('click', chooseMedia);
  q('#convert-start').addEventListener('click', convertMedia);
  document.addEventListener('click', (event) => { const b=event.target.closest('[data-cancel]'); if(b) cancelJob(b.dataset.cancel); });
  if (window.runtime && typeof window.runtime.EventsOn === 'function') window.runtime.EventsOn('openfetch:update', refresh);
  if (!api()) { q('#preview-notice').classList.remove('hidden'); q('#runtime-label').textContent = 'Prévia visual'; }
  else refresh();
  setInterval(refresh, 1800);
}

document.addEventListener('DOMContentLoaded', init);
