import './style.css';
import * as api from './api';
import type { Collection, CollectionChild, CollectionView, DuplicateGroup, Model } from './api';
import type { Viewer } from './viewer';

const PAGE_SIZE = 120;
const POLL_MS = 3000;

const state = {
  /** The route key the current view was loaded for: a collection id or 'root'. */
  viewKey: undefined as number | 'root' | undefined,
  view: undefined as CollectionView | undefined,
  query: '',
  tag: '',
  models: [] as Model[],
  total: 0,
  /** Models come from anywhere beneath the folder (search or tag filter). */
  flat: false,
  /** Show non-model files and folders holding only them. Remembered. */
  showFiles: localStorage.getItem('stilton.showFiles') === '1',
  sort: (localStorage.getItem('stilton.sort') ?? 'name') as api.Sort,
  /** Selected cards, keyed "m:<id>" for models and "c:<id>" for collections. */
  selected: new Map<string, Item>(),
};

/** A selectable card: a model ("m") or collection ("c"). */
interface Item {
  kind: 'm' | 'c';
  id: number;
  name: string;
  /** A model file's extension, e.g. ".stl"; renames keep it. */
  ext?: string;
}

let pollTimer: number | undefined;
/** Items being dragged, while a drag that started on a card is in progress. */
let dragging: Item[] | null = null;
let viewer: Viewer | undefined;

const sameTag = (a: string, b: string) => a.toLowerCase() === b.toLowerCase();

// ---- tiny DOM helper --------------------------------------------------------

type Child = Node | string | null | undefined | false;

function h<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props: Partial<HTMLElementTagNameMap[K]> & { class?: string } = {},
  ...children: Child[]
): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  const { class: className, ...rest } = props;
  if (className) el.className = className;
  Object.assign(el, rest);
  for (const c of children) if (c) el.append(c);
  return el;
}

function folderIcon() {
  const ns = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('viewBox', '0 0 16 16');
  svg.setAttribute('class', 'folder-icon');
  const path = document.createElementNS(ns, 'path');
  path.setAttribute('d', 'M1.5 3.5a1 1 0 0 1 1-1h3.6l1.5 1.5h5.9a1 1 0 0 1 1 1v7.5a1 1 0 0 1-1 1h-11a1 1 0 0 1-1-1z');
  svg.append(path);
  return svg;
}

// ---- layout -----------------------------------------------------------------

const search = h('input', { type: 'search', placeholder: 'Search models…', class: 'search' });
const rescanButton = h('button', { class: 'button' }, 'Rescan');
const dupesAlert = h('a', { class: 'dupes-alert', href: '#/duplicates', hidden: true });
const trashLink = h('a', { class: 'trash-link', href: '#/trash', title: 'Deleted files you can restore' }, 'Trash');
const status = h('span', { class: 'status' });
const crumbs = h('nav', { class: 'crumbs' });
const sortSelect = h(
  'select',
  { class: 'field-input sort-select', title: 'Sort' },
  h('option', { value: 'name' }, 'Name'),
  h('option', { value: 'downloaded' }, 'Recently downloaded'),
  h('option', { value: 'added' }, 'Recently added'),
);
sortSelect.value = state.sort;
const filesToggle = h('input', { type: 'checkbox', checked: state.showFiles });
const viewControls = h(
  'div',
  { class: 'view-controls' },
  h('label', { class: 'toggle', title: 'Images, notes, project files and other non-model files' }, filesToggle, 'Show other files'),
  sortSelect,
);
const details = h('section', { class: 'collection-details' });
const notice = h('p', { class: 'notice', hidden: true });
const tagBar = h('nav', { class: 'tags' });
const grid = h('main', { class: 'grid' });
const moreButton = h('button', { class: 'button more', hidden: true }, 'Load more');
const modal = h('div', { class: 'modal', hidden: true });
const folderName = h('input', { class: 'field-input', placeholder: 'New folder name' });
const createButton = h('button', { class: 'button primary', disabled: true }, 'Move into new folder');
const selectionCount = h('span', { class: 'selection-count' });
const selectionDest = h('span', { class: 'selection-dest' });
const selectionError = h('span', { class: 'field-error', hidden: true });
const moveButton = h('button', { class: 'button', onclick: () => openPicker() }, 'Move to…');
const renameButton = h('button', { class: 'button', hidden: true, onclick: () => openRename([...state.selected.values()][0]) }, 'Rename');
const deleteButton = h('button', { class: 'button danger', onclick: () => openDelete() }, 'Delete');
const toast = h('div', { class: 'toast', hidden: true });
const selectionBar = h(
  'div',
  { class: 'selection-bar', hidden: true },
  selectionCount,
  renameButton,
  moveButton,
  h('span', { class: 'sep' }, 'or'),
  folderName,
  createButton,
  selectionDest,
  selectionError,
  deleteButton,
  h('button', { class: 'button', onclick: () => clearSelection() }, 'Clear'),
);
const browse = h(
  'div',
  {},
  h('div', { class: 'crumb-row' }, crumbs, viewControls),
  details,
  tagBar,
  notice,
  grid,
  moreButton,
  selectionBar,
);
/** Full-page views (duplicates, trash) shown instead of the library. */
const page = h('section', { class: 'page', hidden: true });
const picker = h('div', { class: 'modal', hidden: true });

document.querySelector('#app')!.append(
  h(
    'header',
    { class: 'header' },
    h('h1', {}, h('a', { class: 'brand', href: '#/', title: 'Library' }, h('span', {}, 'STL'), 'ton')),
    search,
    status,
    dupesAlert,
    trashLink,
    rescanButton,
  ),
  browse,
  page,
  modal,
  picker,
  toast,
);

// ---- library grid -----------------------------------------------------------

function collectionHash(c: { id: number; parentId: number | null }) {
  return c.parentId === null ? '#/' : `#/c/${c.id}`;
}

/** Shows a collection, unless it's already the one on screen. */
async function showCollection(key: number | 'root') {
  if (key !== state.viewKey) await loadView(key);
}

/** Loads a collection and its models for the current tag filter. */
async function loadView(key: number | 'root') {
  let view: CollectionView;
  try {
    view = await api.getCollection(key, state.tag, viewOptions());
  } catch (err) {
    console.warn(err);
    if (key !== 'root') location.hash = '#/'; // e.g. folder pruned by a rescan
    return;
  }
  const moved = key !== state.viewKey;
  clearSelection();
  state.view = view;
  state.viewKey = key;
  if (moved) window.scrollTo(0, 0);
  renderCrumbs();
  renderDetails();
  await loadModels();
}

function listParams(limit: number, offset: number): api.ListParams {
  return { collection: state.view?.collection.id, q: state.query, tag: state.tag, limit, offset, ...viewOptions() };
}

function viewOptions(): api.ViewOptions {
  return { files: state.showFiles, sort: state.sort };
}

async function loadModels(append = false) {
  if (!state.view) return;
  const offset = append ? state.models.length : 0;
  const res = await api.listModels(listParams(PAGE_SIZE, offset));
  state.models = append ? [...state.models, ...res.models] : res.models;
  state.total = res.total;
  state.flat = res.flat;
  renderGrid();
  schedulePoll();
}

function renderCrumbs() {
  const chain = state.view?.breadcrumbs ?? [];
  crumbs.replaceChildren(
    ...chain.flatMap((c, i) => [
      ...(i > 0 ? [h('span', { class: 'sep' }, '/')] : []),
      i === chain.length - 1
        ? h('span', { class: 'current' }, c.name)
        : dropTarget(h('a', { href: collectionHash(c) }, c.name), c.id, c.name),
    ]),
  );
  const current = state.view?.collection;
  search.placeholder = current && current.parentId !== null ? `Search in ${current.name}…` : 'Search models…';
}

/** Tags and source link for the open folder (the root has neither). */
function renderDetails() {
  const c = state.view?.collection;
  if (!c || c.parentId === null) {
    details.replaceChildren();
    details.hidden = true;
  } else {
    details.hidden = false;
    details.replaceChildren(
      ...editableFields(c, async (patch) => {
        const updated = await api.patchCollection(c.id, patch);
        c.tags = updated.tags;
        c.sourceUrl = updated.sourceUrl;
        if (patch.tags) {
          loadTags();
          // The folder may have started or stopped matching the active filter.
          if (state.tag) loadView(state.viewKey!);
        }
        return updated;
      }),
      h(
        'div',
        { class: 'download-row' },
        h('a', { class: 'button', href: api.collectionDownloadURL(c.id), title: 'Everything in this folder, as a zip' }, 'Download folder'),
        h('span', { class: 'dupe-meta' }, downloadSummary(c)),
      ),
    );
  }
  renderNotice();
}

/** Inside a folder that carries the filter tag, everything matches; say so. */
function renderNotice() {
  const tagged = state.view?.inTagged && state.tag
    ? state.view.breadcrumbs.find((b) => b.tags.some((t) => sameTag(t, state.tag)))
    : undefined;
  notice.hidden = !tagged;
  if (tagged) {
    notice.replaceChildren(
      'Everything in ',
      h('strong', {}, tagged.name),
      ' is tagged ',
      h('strong', {}, tagged.tags.find((t) => sameTag(t, state.tag))!),
      ', so this folder is shown in full.',
    );
  }
}

function renderGrid() {
  const folders = state.query ? [] : (state.view?.children ?? []);
  grid.replaceChildren(
    ...folders.map((c) => selectable(folderCard(c), { kind: 'c', id: c.id, name: c.name })),
    ...state.models.map((m) =>
      selectable(modelCard(m), { kind: 'm', id: m.id, ...splitExt(m) }),
    ),
  );
  if (folders.length === 0 && state.models.length === 0) {
    grid.append(h('p', { class: 'empty' }, state.query || state.tag ? 'No matches.' : 'No models indexed yet.'));
  }
  status.textContent = [folders.length && plural(folders.length, 'folder'), plural(state.total, state.showFiles ? 'item' : 'model')]
    .filter(Boolean)
    .join(' · ');
  moreButton.hidden = state.models.length >= state.total;
}

function folderCard(c: CollectionChild) {
  let art: HTMLElement;
  if (c.previews.length === 0) {
    art = h('div', { class: 'placeholder' }, c.pendingCount ? 'Rendering…' : 'No previews');
  } else if (c.previews.length === 1) {
    art = h('img', { src: c.previews[0], alt: '', loading: 'lazy' });
  } else {
    const cells = [0, 1, 2, 3].map((i) =>
      c.previews[i] ? h('img', { src: c.previews[i], alt: '', loading: 'lazy' }) : h('div', { class: 'cell' }),
    );
    art = h('div', { class: 'mosaic' }, ...cells);
  }
  // Tag matches can come from anywhere beneath, so say where.
  const location = state.view?.tagMatches ? relativeFolder(c.path.split('/').slice(0, -1).join('/')) : '';
  return h(
    'a',
    { class: 'card folder', href: `#/c/${c.id}`, title: c.path },
    h('div', { class: 'thumb' }, art),
    h(
      'div',
      { class: 'name' },
      folderIcon(),
      h('span', { class: 'label' }, c.name),
      h(
        'span',
        { class: 'count', title: state.showFiles ? `${plural(c.modelCount, 'model')}, ${plural(c.fileCount, 'other file')}` : '' },
        String(c.modelCount + (state.showFiles ? c.fileCount : 0)),
      ),
    ),
    location && h('div', { class: 'location' }, location),
  );
}

/** A folder path relative to the open folder ("" if it is the open folder). */
function relativeFolder(folder: string) {
  const base = state.view?.collection.path;
  if (!base) return folder;
  if (folder === base) return '';
  return folder.startsWith(base + '/') ? folder.slice(base.length + 1) : folder;
}

/** A model's name for renaming, and the extension a rename keeps. */
function splitExt(m: Model) {
  const base = m.path.slice(m.path.lastIndexOf('/') + 1);
  const dot = base.lastIndexOf('.');
  return dot > 0 ? { name: base.slice(0, dot), ext: base.slice(dot) } : { name: base, ext: '' };
}

function fileIcon(m: Model) {
  return h('div', { class: 'placeholder file-icon' }, h('span', {}, m.format ? `.${m.format}` : 'file'));
}

function modelCard(m: Model) {
  const preview = m.thumbnail
    ? h('img', { src: m.thumbnail, alt: m.name, loading: 'lazy' })
    : m.thumbState === 'none'
      ? fileIcon(m)
      : h('div', { class: 'placeholder' }, m.thumbState === 'failed' ? 'No preview' : 'Rendering…');
  if (m.thumbState === 'failed' && m.thumbError) preview.title = m.thumbError;
  // Flattened results come from many folders, so say where each one lives.
  const folder = state.flat ? relativeFolder(m.path.split('/').slice(0, -1).join('/')) : '';
  return h(
    'a',
    { class: `card${m.kind === 'file' ? ' file' : ''}`, href: `#/model/${m.id}`, title: m.path },
    h(
      'div',
      { class: 'thumb' },
      preview,
      h('span', { class: 'badge' }, m.format),
      m.copies > 1 && h('span', { class: 'badge dupe', title: `${m.copies} identical copies` }, `×${m.copies}`),
    ),
    h('div', { class: 'name' }, h('span', { class: 'label' }, m.name)),
    folder && h('div', { class: 'location' }, folder),
  );
}

filesToggle.addEventListener('change', () => {
  state.showFiles = filesToggle.checked;
  localStorage.setItem('stilton.showFiles', state.showFiles ? '1' : '0');
  if (state.viewKey !== undefined) loadView(state.viewKey);
});
sortSelect.addEventListener('change', () => {
  state.sort = sortSelect.value as api.Sort;
  localStorage.setItem('stilton.sort', state.sort);
  if (state.viewKey !== undefined) loadView(state.viewKey);
});

// ---- selection & new folders ------------------------------------------------

/**
 * Wraps a card with a selection checkbox. The checkbox sits beside the link
 * rather than inside it so ticking it doesn't navigate. While anything is
 * selected, clicking a card toggles it instead of opening it.
 */
function selectable(card: HTMLAnchorElement, item: Item) {
  const key = `${item.kind}:${item.id}`;
  const box = h('input', { type: 'checkbox', checked: state.selected.has(key), title: 'Select' });
  const wrap = h('div', { class: `card-wrap${box.checked ? ' selected' : ''}` }, card, h('label', { class: 'select' }, box));
  const toggle = (on: boolean) => {
    if (on) state.selected.set(key, item);
    else state.selected.delete(key);
    box.checked = on;
    wrap.classList.toggle('selected', on);
    renderSelectionBar();
  };
  box.addEventListener('change', () => toggle(box.checked));
  card.addEventListener('click', (e) => {
    if (state.selected.size === 0 || e.ctrlKey || e.metaKey) return;
    e.preventDefault();
    toggle(!state.selected.has(key));
  });

  // Dragging a selected card drags the whole selection; otherwise just it.
  wrap.addEventListener('dragstart', (e) => {
    dragging = state.selected.has(key) ? [...state.selected.values()] : [item];
    e.dataTransfer!.effectAllowed = 'move';
    e.dataTransfer!.setData('application/x-stlton', String(dragging.length));
    if (dragging.length > 1) {
      const ghost = h('div', { class: 'drag-ghost' }, `${dragging.length} items`);
      document.body.append(ghost);
      e.dataTransfer!.setDragImage(ghost, 16, 16);
      setTimeout(() => ghost.remove());
    }
    const keys = new Set(dragging.map((i) => `${i.kind}:${i.id}`));
    grid.querySelectorAll<HTMLElement>('.card-wrap').forEach((w) => {
      if (keys.has(w.dataset.key!)) w.classList.add('dragging');
    });
  });
  wrap.addEventListener('dragend', endDrag);
  wrap.dataset.key = key;
  if (item.kind === 'c') dropTarget(wrap, item.id, item.name);
  return wrap;
}

function endDrag() {
  dragging = null;
  document.querySelectorAll('.dragging, .drop-target').forEach((el) => el.classList.remove('dragging', 'drop-target'));
}

/** Makes el accept dropped cards, moving them into collection destId. */
function dropTarget<T extends HTMLElement>(el: T, destId: number, destName: string): T {
  // Only drags that started on a card, and not onto a folder being dragged.
  const accepts = () => dragging !== null && !dragging.some((i) => i.kind === 'c' && i.id === destId);
  el.addEventListener('dragover', (e) => {
    if (!accepts()) return;
    e.preventDefault();
    e.dataTransfer!.dropEffect = 'move';
    el.classList.add('drop-target');
  });
  el.addEventListener('dragleave', (e) => {
    if (!el.contains(e.relatedTarget as Node | null)) el.classList.remove('drop-target');
  });
  el.addEventListener('drop', async (e) => {
    if (!accepts()) return;
    e.preventDefault();
    const items = dragging!;
    endDrag();
    try {
      await moveItems(items, destId, destName);
    } catch (err) {
      showToast(err instanceof api.ApiError ? err.detail : (err as Error).message, true);
    }
  });
  return el;
}

function showToast(message: string, error = false) {
  toast.textContent = message;
  toast.classList.toggle('error', error);
  toast.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => (toast.hidden = true), error ? 6000 : 3000);
}
let toastTimer: number | undefined;

function renderSelectionBar() {
  const n = state.selected.size;
  selectionBar.hidden = n === 0;
  document.body.classList.toggle('selecting', n > 0);
  selectionCount.textContent = `${n} selected`;
  renameButton.hidden = n !== 1;
  const here = state.view?.collection;
  selectionDest.textContent = here ? `in ${here.parentId === null ? 'Library' : here.name}` : '';
  createButton.disabled = folderName.value.trim() === '';
}

function clearSelection() {
  state.selected.clear();
  grid.querySelectorAll('.card-wrap.selected').forEach((el) => {
    el.classList.remove('selected');
    el.querySelector<HTMLInputElement>('.select input')!.checked = false;
  });
  folderName.value = '';
  selectionError.hidden = true;
  renderSelectionBar();
}

folderName.addEventListener('input', renderSelectionBar);
folderName.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !createButton.disabled) createButton.click();
});

createButton.addEventListener('click', async () => {
  const view = state.view;
  if (!view) return;
  const items = [...state.selected.values()];
  createButton.disabled = true;
  try {
    const created = await api.createCollection({
      parent: view.collection.id,
      name: folderName.value.trim(),
      models: items.filter((i) => i.kind === 'm').map((i) => i.id),
      collections: items.filter((i) => i.kind === 'c').map((i) => i.id),
    });
    await afterMove(items.length, created.name);
  } catch (err) {
    selectionError.textContent = err instanceof api.ApiError ? err.detail : (err as Error).message;
    selectionError.hidden = false;
    createButton.disabled = false;
  }
});

/** Moves the selection, reloads the view, and reports the outcome. */
async function afterMove(items: number, dest: string) {
  await loadView(state.viewKey ?? 'root');
  loadTags(); // moved folders may have taken the last use of a tag with them
  status.textContent = `Moved ${items} item${items === 1 ? '' : 's'} into ${dest}`;
}

/** Moves items into an existing collection; throws if the server refuses. */
async function moveItems(items: Item[], destId: number, destName: string) {
  await api.moveInto(destId, selectionIds(items));
  await afterMove(items.length, destName);
}

/** Splits the selection into the ids the API expects. */
function selectionIds(items: Item[]) {
  return {
    models: items.filter((i) => i.kind === 'm').map((i) => i.id),
    collections: items.filter((i) => i.kind === 'c').map((i) => i.id),
  };
}

/** "3 times · last 2 days ago", or "Never". */
function downloadSummary(x: { downloadCount: number; lastDownloadedAt: number | null }) {
  if (!x.downloadCount || x.lastDownloadedAt === null) return 'Never downloaded';
  return `${plural(x.downloadCount, 'download')} · last ${timeAgo(x.lastDownloadedAt)}`;
}

function timeAgo(unix: number) {
  const s = Date.now() / 1000 - unix;
  if (s < 60) return 'just now';
  const units: [number, string][] = [[60, 'minute'], [3600, 'hour'], [86400, 'day'], [2592000, 'month'], [31536000, 'year']];
  let [size, name] = units[0];
  for (const u of units) if (s >= u[0]) [size, name] = u;
  return `${plural(Math.floor(s / size), name)} ago`;
}

const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`;

/** Confirmation dialog that previews exactly what a delete will remove. */
async function openDelete() {
  const items = [...state.selected.values()];
  const sel = selectionIds(items);
  const panel = h('div', { class: 'picker' }, h('p', { class: 'empty' }, 'Counting files…'));
  picker.replaceChildren(panel);
  picker.hidden = false;

  let plan: api.DeletePlan;
  try {
    plan = await api.previewDelete(sel);
  } catch (err) {
    panel.replaceChildren(
      h('p', { class: 'field-error' }, err instanceof api.ApiError ? err.detail : (err as Error).message),
      h('div', { class: 'picker-actions' }, h('button', { class: 'button', onclick: () => closePicker() }, 'Close')),
    );
    return;
  }
  if (picker.hidden) return; // closed while counting

  const error = h('p', { class: 'field-error', hidden: true });
  const confirmButton = h('button', { class: 'button danger solid' }, `Move ${plural(plan.files, 'file')} to trash`);
  confirmButton.addEventListener('click', async () => {
    confirmButton.disabled = true;
    try {
      const done = await api.deleteItems(sel, plan.files);
      closePicker();
      await loadView(state.viewKey ?? 'root');
      loadTags();
      refreshDupesAlert();
      refreshTrashLink();
      status.textContent = `Moved ${plural(done.files, 'file')} to the trash`;
    } catch (err) {
      error.textContent = err instanceof api.ApiError ? err.detail : (err as Error).message;
      error.hidden = false;
      // If the contents changed, the dialog's counts are stale: start over.
      if (err instanceof api.ApiError && err.status === 409) {
        confirmButton.textContent = 'Review again';
        confirmButton.onclick = () => openDelete();
      }
      confirmButton.disabled = false;
    }
  });

  const breakdown = [
    plural(plan.modelFiles, 'model file'),
    plan.otherFiles > 0 && plural(plan.otherFiles, 'other file'),
  ].filter(Boolean).join(' and ');
  panel.replaceChildren(
    h('h2', {}, `Delete ${plural(plan.files, 'file')}?`),
    h(
      'ul',
      { class: 'delete-list' },
      ...plan.items.map((it) =>
        h(
          'li',
          {},
          it.kind === 'collection' ? folderIcon() : null,
          h('span', { class: 'label', title: it.path }, it.path),
          h('span', { class: 'count' }, it.kind === 'collection' ? plural(it.files, 'file') : formatBytes(it.bytes)),
        ),
      ),
    ),
    h(
      'p',
      { class: 'delete-summary' },
      `${breakdown}`,
      plan.folders > 0 ? `, in ${plural(plan.folders, 'folder')}` : '',
      ` · ${formatBytes(plan.bytes)}`,
    ),
    ...(plan.otherFiles > 0
      ? [h(
          'p',
          { class: 'delete-note' },
          `Folders are deleted with everything in them, including ${plural(plan.otherFiles, 'file')} STLton doesn't show (notes, images, project files).`,
        )]
      : []),
    h('p', { class: 'delete-note' }, 'They’re moved to the Trash, where you can restore them until it’s emptied.'),
    error,
    h('div', { class: 'picker-actions' }, h('button', { class: 'button', onclick: () => closePicker() }, 'Cancel'), confirmButton),
  );
}

/** Dialog for renaming a single model file or folder. */
function openRename(item: Item) {
  const input = h('input', { class: 'field-input', value: item.name });
  const error = h('p', { class: 'field-error', hidden: true });
  const save = h('button', { class: 'button primary' }, 'Rename');
  const submit = async () => {
    const name = input.value.trim();
    if (name === item.name) return closePicker();
    save.disabled = true;
    try {
      if (item.kind === 'm') await api.patchModel(item.id, { name });
      else await api.patchCollection(item.id, { name });
      closePicker();
      await loadView(state.viewKey ?? 'root');
      status.textContent = `Renamed to ${name}`;
    } catch (err) {
      error.textContent = err instanceof api.ApiError ? err.detail : (err as Error).message;
      error.hidden = false;
      save.disabled = false;
    }
  };
  save.addEventListener('click', submit);
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') submit();
  });

  picker.replaceChildren(
    h(
      'div',
      { class: 'picker' },
      h('h2', {}, `Rename ${item.kind === 'c' ? 'folder' : 'file'}`),
      h('div', { class: 'field-row' }, input, item.ext && h('span', { class: 'ext' }, item.ext)),
      error,
      h('div', { class: 'picker-actions' }, h('button', { class: 'button', onclick: () => closePicker() }, 'Cancel'), save),
    ),
  );
  picker.hidden = false;
  input.focus();
  input.select();
}

/** Folder browser for choosing where to move the selection. */
async function openPicker(at: number | 'root' = state.view?.collection.id ?? 'root') {
  const items = [...state.selected.values()];
  const movingFolders = new Set(items.filter((i) => i.kind === 'c').map((i) => i.id));
  picker.hidden = false;
  const panel = h('div', { class: 'picker' }, h('p', { class: 'empty' }, 'Loading…'));
  picker.replaceChildren(panel);

  let view: CollectionView;
  try {
    view = await api.getCollection(at);
  } catch (err) {
    panel.replaceChildren(h('p', { class: 'field-error' }, (err as Error).message));
    return;
  }
  if (picker.hidden) return; // closed while loading

  const here = view.collection;
  // Can't move a folder into itself or anything inside it.
  const blocked = view.breadcrumbs.find((b) => movingFolders.has(b.id));
  const error = h('p', { class: 'field-error', hidden: true });
  const moveHere = h('button', { class: 'button primary', disabled: !!blocked }, 'Move here');
  moveHere.addEventListener('click', async () => {
    moveHere.disabled = true;
    try {
      await moveItems(items, here.id, here.name);
      closePicker();
    } catch (err) {
      error.textContent = err instanceof api.ApiError ? err.detail : (err as Error).message;
      error.hidden = false;
      moveHere.disabled = false;
    }
  });

  const rows = view.children.map((c) => {
    const moving = movingFolders.has(c.id);
    const row = h(
      'button',
      { class: 'picker-row', disabled: moving, title: moving ? 'This folder is being moved' : c.path },
      folderIcon(),
      h('span', { class: 'label' }, c.name),
      h('span', { class: 'count' }, moving ? 'moving' : String(c.modelCount)),
    );
    row.addEventListener('click', () => openPicker(c.id));
    return row;
  });

  panel.replaceChildren(
    h('h2', {}, `Move ${items.length} item${items.length === 1 ? '' : 's'} to…`),
    h(
      'nav',
      { class: 'crumbs' },
      ...view.breadcrumbs.flatMap((b, i) => {
        const last = i === view.breadcrumbs.length - 1;
        const crumb = last
          ? h('span', { class: 'current' }, b.name)
          : h('a', { href: '#', onclick: (e: Event) => { e.preventDefault(); openPicker(b.id); } }, b.name);
        return i > 0 ? [h('span', { class: 'sep' }, '/'), crumb] : [crumb];
      }),
    ),
    h('div', { class: 'picker-list' }, ...(rows.length ? rows : [h('p', { class: 'empty' }, 'No folders inside.')])),
    ...(blocked ? [h('p', { class: 'field-error' }, `${blocked.name} is being moved, so it can't be the destination.`)] : []),
    error,
    h(
      'div',
      { class: 'picker-actions' },
      h('button', { class: 'button', onclick: () => closePicker() }, 'Cancel'),
      moveHere,
    ),
  );
  moveHere.focus();
}

function closePicker() {
  picker.hidden = true;
  picker.replaceChildren();
}

picker.addEventListener('click', (e) => {
  if (e.target === picker) closePicker();
});

// While thumbnails are still being rendered, refresh what's on screen so they
// pop in. Finished thumbnails are cached by the browser, so this is cheap.
function schedulePoll() {
  clearTimeout(pollTimer);
  const pending =
    state.models.some((m) => m.thumbState === 'pending') ||
    (!state.query && state.view?.children.some((c) => c.pendingCount > 0));
  if (!pending || !state.view) return;
  const view = state.view;
  pollTimer = window.setTimeout(async () => {
    const [fresh, res] = await Promise.all([
      api.getCollection(view.collection.id, state.tag, viewOptions()),
      api.listModels(listParams(Math.max(state.models.length, 1), 0)),
    ]);
    if (state.view !== view) return; // navigated or refiltered meanwhile
    state.view = fresh;
    state.models = res.models;
    state.total = res.total;
    state.flat = res.flat;
    renderGrid();
    schedulePoll();
  }, POLL_MS);
}

async function loadTags() {
  const tags = await api.listTags();
  const chip = (name: string, label: string) =>
    h('button', {
      class: `chip${state.tag === name ? ' active' : ''}`,
      onclick: () => {
        state.tag = name;
        loadTags();
        if (state.viewKey !== undefined) loadView(state.viewKey);
      },
    }, label);
  // A filter on a tag that no longer exists would match nothing; drop it.
  if (state.tag && !tags.some((t) => sameTag(t.name, state.tag))) {
    state.tag = '';
    if (state.viewKey !== undefined) loadView(state.viewKey);
  }
  tagBar.replaceChildren(
    ...(tags.length ? [chip('', 'All')] : []),
    ...tags.map((t) => chip(t.name, `${t.name} · ${t.models + t.collections}`)),
  );
}

let searchDebounce: number | undefined;
search.addEventListener('input', () => {
  clearTimeout(searchDebounce);
  searchDebounce = window.setTimeout(() => {
    clearSelection();
    state.query = search.value.trim();
    loadModels();
  }, 200);
});

moreButton.addEventListener('click', () => loadModels(true));

rescanButton.addEventListener('click', async () => {
  rescanButton.disabled = true;
  status.textContent = 'Scanning…';
  try {
    const r = await api.rescan();
    status.title = '';
    // Reload the current folder; it may have gained, lost, or been removed.
    await loadView(state.viewKey ?? 'root');
    loadTags();
    refreshDupesAlert();
    status.textContent = `+${r.added} ~${r.updated} -${r.removed}`;
  } catch (err) {
    status.textContent = 'Scan failed';
    status.title = (err as Error).message;
  } finally {
    rescanButton.disabled = false;
  }
});

// ---- tags & source link editing ---------------------------------------------

type Editable = Pick<Model | Collection, 'tags' | 'sourceUrl'>;

/**
 * Inline editors for an item's tags and source link, each saved when the
 * input changes. save applies a patch and returns the updated item.
 */
function editableFields(item: Editable, save: (patch: api.Patch) => Promise<Editable>) {
  const tagInput = h('input', { class: 'field-input', value: item.tags.join(', '), placeholder: 'Comma separated' });
  const sourceInput = h('input', {
    class: 'field-input',
    type: 'url',
    value: item.sourceUrl,
    placeholder: 'Where it came from, e.g. printables.com/…',
  });
  const sourceLink = h('a', { class: 'source-link', target: '_blank', rel: 'noopener noreferrer', title: 'Open source link' }, '↗');
  const error = h('p', { class: 'field-error', hidden: true });

  const show = (it: Editable) => {
    tagInput.value = it.tags.join(', ');
    sourceInput.value = it.sourceUrl;
    sourceLink.href = it.sourceUrl;
    sourceLink.hidden = !it.sourceUrl;
  };
  show(item);

  const submit = async (patch: api.Patch, input: HTMLInputElement) => {
    try {
      show(await save(patch));
      error.hidden = true;
      input.classList.remove('invalid');
    } catch (err) {
      error.textContent = err instanceof api.ApiError ? err.detail : (err as Error).message;
      error.hidden = false;
      input.classList.add('invalid');
    }
  };
  tagInput.addEventListener('change', () => {
    const tags = tagInput.value.split(',').map((t) => t.trim()).filter(Boolean);
    if (tags.join(',') !== item.tags.join(',')) submit({ tags }, tagInput);
  });
  sourceInput.addEventListener('change', () => {
    if (sourceInput.value.trim() !== item.sourceUrl) submit({ sourceUrl: sourceInput.value }, sourceInput);
  });

  return [
    h('label', { class: 'field' }, h('span', {}, 'Tags'), tagInput),
    h('label', { class: 'field' }, h('span', {}, 'Source'), h('span', { class: 'field-row' }, sourceInput, sourceLink)),
    error,
  ];
}

// ---- model detail -----------------------------------------------------------

async function openModel(id: number) {
  const m = await api.getModel(id);
  // Deep link: show the model's folder behind the dialog.
  if (!state.view) showCollection(m.collectionId);

  const stage = h('div', { class: 'stage' }, h('div', { class: 'loading' }, 'Loading…'));

  const close = h('a', { class: 'close', href: '#/', title: 'Close (Esc)' }, '×');
  close.addEventListener('click', (e) => {
    e.preventDefault();
    closeModelRoute();
  });

  modal.replaceChildren(
    h(
      'div',
      { class: 'panel' },
      stage,
      h(
        'aside',
        { class: 'details' },
        close,
        h('h2', {}, m.name),
        m.copies > 1 &&
          h(
            'p',
            { class: 'dupe-note' },
            `Identical to ${m.copies - 1} other file${m.copies === 2 ? '' : 's'}. `,
            h('a', { href: '#/duplicates' }, 'Choose which to keep'),
          ),
        h('dl', {},
          h('dt', {}, 'Path'), h('dd', {}, m.path),
          h('dt', {}, 'Format'), h('dd', {}, m.format.toUpperCase()),
          h('dt', {}, 'Size'), h('dd', {}, formatBytes(m.size)),
          h('dt', {}, 'Modified'), h('dd', {}, new Date(m.mtime * 1000).toLocaleString()),
          h('dt', {}, 'Downloads'), h('dd', {}, downloadSummary(m)),
        ),
        ...editableFields(m, async (patch) => {
          const updated = await api.patchModel(m.id, patch);
          m.tags = updated.tags;
          m.sourceUrl = updated.sourceUrl;
          if (patch.tags) {
            loadTags();
            if (state.tag && state.viewKey !== undefined) loadView(state.viewKey);
          }
          return updated;
        }),
        h('a', { class: 'button', href: `${api.fileURL(m.id)}?download` }, 'Download'),
      ),
    ),
  );
  modal.hidden = false;

  viewer?.dispose();
  viewer = undefined;
  if (m.kind === 'file') {
    // Not a model: show images as they are, anything else as an icon.
    stage.replaceChildren(
      api.isImage(m)
        ? h('img', { class: 'file-preview', src: api.fileURL(m.id), alt: m.name })
        : h('div', { class: 'file-stage' }, fileIcon(m), h('p', {}, 'No preview for this kind of file')),
    );
    return;
  }
  // three.js is only needed here, so it's loaded on first open rather than with the grid.
  const { Viewer } = await import('./viewer');
  if (!stage.isConnected) return; // closed or replaced while loading
  const v = (viewer = new Viewer(stage));
  try {
    if (!(await loadInto(v, m))) throw new Error('no geometry found');
    stage.querySelector('.loading')?.remove();
  } catch (err) {
    if (viewer === v) stage.querySelector('.loading')!.textContent = `Couldn't load model: ${(err as Error).message}`;
  }
}

/**
 * Loads the original file; for 3MFs three.js can't assemble (e.g. Bambu
 * Studio's multi-part packages), falls back to the server's merged STL.
 */
async function loadInto(v: Viewer, m: Model): Promise<boolean> {
  const data = await fetchBytes(api.fileURL(m.id));
  if (m.format !== '3mf') return v.load(data, m.format);
  try {
    if (v.load(data, '3mf')) return true;
  } catch (err) {
    console.warn('3MFLoader failed, using server-merged mesh', err);
  }
  return v.load(await fetchBytes(api.meshURL(m.id)), 'stl');
}

async function fetchBytes(url: string) {
  const res = await fetch(url);
  if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);
  return res.arrayBuffer();
}

function closeModel() {
  viewer?.dispose();
  viewer = undefined;
  modal.hidden = true;
  modal.replaceChildren();
}

/** Closes the dialog by returning to the folder behind it. */
function closeModelRoute() {
  location.hash = state.view ? collectionHash(state.view.collection) : '#/';
}

function formatBytes(n: number) {
  const units = ['B', 'KB', 'MB', 'GB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

// ---- duplicates -------------------------------------------------------------

/** The header nags until every set of duplicates has been resolved. */
async function refreshDupesAlert() {
  const { total } = await api.listDuplicates(0);
  dupesAlert.hidden = total === 0;
  dupesAlert.textContent = `${total} duplicate set${total === 1 ? '' : 's'} to resolve`;
}

/** Swaps the library view for a full page. */
function showPage() {
  browse.hidden = true;
  search.hidden = true;
  page.hidden = false;
  window.scrollTo(0, 0);
  page.replaceChildren(h('p', { class: 'empty' }, 'Loading…'));
}

async function showDuplicates() {
  showPage();
  const { groups, total } = await api.listDuplicates(200);
  status.textContent = `${total} set${total === 1 ? '' : 's'} of duplicates`;
  page.replaceChildren(
    h('h2', {}, 'Duplicate files'),
    h(
      'p',
      { class: 'dupes-intro' },
      'Each set below is the same file stored more than once. Pick the copy to keep: the others are ',
      h('strong', {}, 'moved to the Trash'),
      '. Their tags and source links are copied to the one you keep, and folders left empty are removed.',
    ),
    ...(groups.length ? groups.map(duplicateGroup) : [h('p', { class: 'empty' }, 'No duplicates. Nice and tidy.')]),
  );
  if (total > groups.length) {
    page.append(h('p', { class: 'empty' }, `Showing ${groups.length} of ${total}. Resolve these to see more.`));
  }
}

function duplicateGroup(g: DuplicateGroup) {
  const name = `keep-${g.hash}`;
  let keepId: number | undefined;
  const others = g.models.length - 1;
  const button = h('button', { class: 'button danger', disabled: true }, `Keep selected, trash ${others}`);
  const result = h('p', { class: 'field-error', hidden: true });

  const options = g.models.map((m) => {
    const radio = h('input', { type: 'radio', name, value: String(m.id) });
    radio.addEventListener('change', () => {
      keepId = m.id;
      button.disabled = false;
    });
    const folder = m.path.split('/').slice(0, -1).join('/');
    return h(
      'label',
      { class: 'dupe-option' },
      radio,
      h(
        'span',
        { class: 'dupe-path' },
        h('span', {}, m.path),
        h('span', { class: 'dupe-meta' },
          h('a', { href: `#/c/${m.collectionId}` }, folder ? `in ${folder}` : 'in Library'),
          ...m.tags.map((t) => h('span', { class: 'chip small' }, t)),
          m.sourceUrl && h('span', { class: 'chip small' }, 'has source link'),
        ),
      ),
    );
  });

  const el = h(
    'article',
    { class: 'dupe-group' },
    g.thumbnail ? h('img', { src: g.thumbnail, alt: '', loading: 'lazy' }) : h('div', { class: 'placeholder' }),
    h(
      'div',
      { class: 'dupe-body' },
      h('h3', {}, g.models[0].name, h('span', { class: 'dupe-meta' }, ` · ${formatBytes(g.size)} · ${g.models.length} copies`)),
      ...options,
      h('div', { class: 'dupe-actions' }, button, result),
    ),
  );

  button.addEventListener('click', async () => {
    const keep = g.models.find((m) => m.id === keepId)!;
    const doomed = g.models.filter((m) => m !== keep).map((m) => `  ${m.path}`);
    if (!confirm(`Keep ${keep.path} and move to the trash:\n\n${doomed.join('\n')}`)) return;
    button.disabled = true;
    try {
      const r = await api.resolveDuplicates(g.hash, keep.id);
      state.viewKey = undefined; // folders and models changed; reload when browsing again
      refreshDupesAlert();
      refreshTrashLink();
      loadTags();
      if (r.skipped.length === 0) {
        el.remove();
        return;
      }
      result.hidden = false;
      result.textContent =
        `Trashed ${r.deleted.length}. Skipped ${r.skipped.join(', ')}: changed since the last scan, so no longer identical. Rescan to update.`;
    } catch (err) {
      result.hidden = false;
      result.textContent = err instanceof api.ApiError ? err.detail : (err as Error).message;
      button.disabled = false;
    }
  });
  return el;
}

function hidePage() {
  page.hidden = true;
  page.replaceChildren();
  browse.hidden = false;
  search.hidden = false;
}

// ---- trash ------------------------------------------------------------------

async function refreshTrashLink() {
  const { files } = await api.listTrash();
  trashLink.textContent = files ? `Trash · ${files}` : 'Trash';
}

async function showTrash() {
  showPage();
  const { batches, files, bytes } = await api.listTrash();
  status.textContent = files ? `${plural(files, 'file')} in the trash` : 'Trash is empty';
  const empty = h('button', { class: 'button danger', disabled: batches.length === 0 }, 'Empty trash');
  empty.addEventListener('click', async () => {
    if (!confirm(`Permanently delete ${plural(files, 'file')} (${formatBytes(bytes)})? This can’t be undone.`)) return;
    empty.disabled = true;
    try {
      await api.purgeTrash();
      refreshTrashLink();
      showTrash();
    } catch (err) {
      showToast(err instanceof api.ApiError ? err.detail : (err as Error).message, true);
      empty.disabled = false;
    }
  });
  page.replaceChildren(
    h('div', { class: 'page-head' }, h('h2', {}, 'Trash'), empty),
    h(
      'p',
      { class: 'dupes-intro' },
      'Deleted files and folders wait here, in a hidden ',
      h('code', {}, '.stilton-trash'),
      ' folder inside your library, until you empty the trash. Restoring puts them back where they were, with their tags and source links.',
    ),
    ...(batches.length ? [h('p', { class: 'delete-summary' }, `${plural(files, 'file')} · ${formatBytes(bytes)}`)] : []),
    ...(batches.length ? batches.map(trashBatch) : [h('p', { class: 'empty' }, 'The trash is empty.')]),
  );
}

function trashBatch(b: api.TrashBatch) {
  const error = h('p', { class: 'field-error', hidden: true });
  const fail = (err: unknown) => {
    error.textContent = err instanceof api.ApiError ? err.detail : (err as Error).message;
    error.hidden = false;
  };
  const restore = h('button', { class: 'button primary' }, 'Restore');
  const purge = h('button', { class: 'button danger' }, 'Delete forever');
  restore.addEventListener('click', async () => {
    restore.disabled = true;
    try {
      await api.restoreTrash(b.id);
      state.viewKey = undefined; // library changed; reload when browsing again
      refreshTrashLink();
      refreshDupesAlert();
      loadTags();
      showToast(`Restored ${plural(b.files, 'file')}`);
      showTrash();
    } catch (err) {
      fail(err);
      restore.disabled = false;
    }
  });
  purge.addEventListener('click', async () => {
    if (!confirm(`Permanently delete ${plural(b.files, 'file')}? This can’t be undone.`)) return;
    purge.disabled = true;
    try {
      await api.purgeTrash(b.id);
      refreshTrashLink();
      showTrash();
    } catch (err) {
      fail(err);
      purge.disabled = false;
    }
  });

  const shown = b.items.slice(0, 6);
  return h(
    'article',
    { class: 'trash-batch' },
    h(
      'div',
      { class: 'trash-head' },
      h('h3', {}, b.reason),
      h('span', { class: 'dupe-meta' }, `${new Date(b.deletedAt * 1000).toLocaleString()} · ${plural(b.files, 'file')} · ${formatBytes(b.bytes)}`),
    ),
    h(
      'ul',
      { class: 'delete-list' },
      ...shown.map((it) =>
        h(
          'li',
          {},
          it.kind === 'collection' ? folderIcon() : null,
          h('span', { class: 'label', title: it.path }, it.path),
          h('span', { class: 'count' }, it.kind === 'collection' ? plural(it.files, 'file') : formatBytes(it.bytes)),
        ),
      ),
      ...(b.items.length > shown.length ? [h('li', { class: 'more-items' }, `and ${b.items.length - shown.length} more`)] : []),
    ),
    error,
    h('div', { class: 'dupe-actions' }, restore, purge),
  );
}

// ---- routing ----------------------------------------------------------------
//
//   #/            library root
//   #/c/<id>      a collection (folder)
//   #/model/<id>  a model, shown over its folder
//   #/duplicates  sets of identical files to resolve
//   #/trash       deleted items to restore or purge

function route() {
  if (location.hash === '#/duplicates' || location.hash === '#/trash') {
    closeModel();
    if (location.hash === '#/trash') showTrash();
    else showDuplicates();
    return;
  }
  hidePage();
  const model = location.hash.match(/^#\/model\/(\d+)$/);
  if (model) {
    openModel(Number(model[1]));
    return;
  }
  closeModel();
  const collection = location.hash.match(/^#\/c\/(\d+)$/);
  showCollection(collection ? Number(collection[1]) : 'root');
}

window.addEventListener('hashchange', route);
window.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return;
  if (!picker.hidden) closePicker();
  else if (!modal.hidden) closeModelRoute();
  else if (state.selected.size > 0) clearSelection();
});
modal.addEventListener('click', (e) => {
  if (e.target === modal) closeModelRoute();
});

loadTags();
refreshDupesAlert();
refreshTrashLink();
route();
