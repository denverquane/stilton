/** A model format, or for other files, their lowercase extension. */
export type Format = 'stl' | 'obj' | '3mf' | (string & {});

export interface Model {
  id: number;
  collectionId: number;
  path: string;
  name: string;
  format: Format;
  size: number;
  mtime: number;
  hash: string;
  /** "none" for files with nothing to render. */
  thumbState: 'pending' | 'ok' | 'failed' | 'none';
  thumbError?: string;
  thumbnail?: string;
  addedAt: number;
  sourceUrl: string;
  tags: string[];
  /** Models with identical contents, including this one. */
  copies: number;
  /** "file" for anything that isn't a 3D model (images, notes, project files). */
  kind: 'model' | 'file';
  downloadCount: number;
  /** Unix seconds; null if never downloaded. */
  lastDownloadedAt: number | null;
}

export interface DuplicateGroup {
  hash: string;
  size: number;
  thumbnail?: string;
  models: Model[];
}

export interface ResolveResult {
  deleted: string[];
  /** Copies that changed on disk since the last scan, left alone. */
  skipped: string[];
}

export interface Collection {
  id: number;
  parentId: number | null;
  path: string;
  name: string;
  sourceUrl: string;
  tags: string[];
  downloadCount: number;
  lastDownloadedAt: number | null;
}

export interface CollectionChild extends Collection {
  /** Models anywhere beneath this collection. */
  modelCount: number;
  /** Other files anywhere beneath this collection. */
  fileCount: number;
  pendingCount: number;
  /** Up to four thumbnail URLs from the collection's subtree. */
  previews: string[];
}

export interface CollectionView {
  collection: Collection;
  /** Root first, ending with the collection itself. */
  breadcrumbs: Collection[];
  /**
   * Collections to show as folders: normally the direct children, but when a
   * tag filter is active (and tagMatches is set), the outermost collections
   * carrying the tag anywhere beneath this one.
   */
  children: CollectionChild[];
  tagMatches: boolean;
  /** This collection or an ancestor carries the filter tag, so everything here matches. */
  inTagged: boolean;
}

export interface TagCount {
  name: string;
  models: number;
  collections: number;
}

/** A partial update: omitted fields are left unchanged. */
export interface Patch {
  /** Renames the file (its extension is kept) or folder on disk. */
  name?: string;
  tags?: string[];
  /** http(s) link; "" clears it. A missing scheme is assumed to be https. */
  sourceUrl?: string;
}

export interface ScanResult {
  added: number;
  updated: number;
  removed: number;
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    /** The server's message, suitable for showing to the user. */
    readonly detail: string,
    method: string,
    url: string,
  ) {
    super(`${method} ${url}: ${status} ${detail}`);
  }
}

async function request<T>(url: string, init?: RequestInit): Promise<T> {
  const res = await fetch(url, init);
  if (!res.ok) {
    throw new ApiError(res.status, (await res.text()).trim(), init?.method ?? 'GET', url);
  }
  return res.json() as Promise<T>;
}

export type Sort = 'name' | 'downloaded' | 'added';

/** Options shared by model and collection listings. */
export interface ViewOptions {
  /** Include non-model files (and folders holding only them). */
  files?: boolean;
  sort?: Sort;
}

export interface ListParams extends ViewOptions {
  q?: string;
  tag?: string;
  collection?: number;
  limit?: number;
  offset?: number;
}

export function listModels(params: ListParams) {
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '' && v !== false) qs.set(k, v === true ? '1' : String(v));
  }
  /** flat: models come from anywhere beneath the collection, not just directly in it. */
  return request<{ models: Model[]; total: number; flat: boolean }>(`/api/models?${qs}`);
}

export const getModel = (id: number) => request<Model>(`/api/models/${id}`);

export function getCollection(id: number | 'root', tag = '', opts: ViewOptions = {}) {
  const qs = new URLSearchParams();
  if (tag) qs.set('tag', tag);
  if (opts.files) qs.set('files', '1');
  if (opts.sort) qs.set('sort', opts.sort);
  return request<CollectionView>(`/api/collections/${id}?${qs}`);
}

/** Downloads a folder as a zip; counted as a download. */
export const collectionDownloadURL = (id: number) => `/api/collections/${id}/download`;

export const listDuplicates = (limit = 50, offset = 0) =>
  request<{ groups: DuplicateGroup[]; total: number }>(`/api/duplicates?limit=${limit}&offset=${offset}`);

/** Keeps one copy and deletes the others from disk. */
export const resolveDuplicates = (hash: string, keepId: number) =>
  request<ResolveResult>(`/api/duplicates/${hash}/resolve`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ keepId }),
  });

export const listTags = () => request<TagCount[]>('/api/tags');

const patch = <T>(url: string, body: Patch) =>
  request<T>(url, { method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });

export const patchModel = (id: number, body: Patch) => patch<Model>(`/api/models/${id}`, body);

/**
 * Creates a folder inside parent and moves the given models and collections
 * into it, on disk. Tags and source links move with them.
 */
export const createCollection = (body: { parent: number; name: string; models: number[]; collections: number[] }) =>
  request<Collection>('/api/collections', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });

/** Moves models and collections into an existing collection, on disk. */
export const moveInto = (dest: number | 'root', body: { models: number[]; collections: number[] }) =>
  request<Collection>(`/api/collections/${dest}/move`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });

export interface DeletePlan {
  items: { kind: 'model' | 'collection'; path: string; files: number; bytes: number }[];
  /** Every file that will be removed: modelFiles + otherFiles. */
  files: number;
  modelFiles: number;
  /** Non-model files inside deleted folders, e.g. datapackage.json. */
  otherFiles: number;
  folders: number;
  bytes: number;
}

type Selection = { models: number[]; collections: number[] };

/** What deleting a selection would remove; deletes nothing. */
export const previewDelete = (sel: Selection) =>
  request<DeletePlan>('/api/delete/preview', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(sel),
  });

/**
 * Moves the selection to the trash. expectFiles is the previewed
 * file count; the server refuses if the contents have changed since.
 */
export const deleteItems = (sel: Selection, expectFiles: number) =>
  request<DeletePlan>('/api/delete', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ...sel, expectFiles }),
  });

export interface TrashBatch {
  id: string;
  /** Unix seconds. */
  deletedAt: number;
  reason: string;
  items: DeletePlan['items'];
  files: number;
  bytes: number;
}

export const listTrash = () => request<{ batches: TrashBatch[]; files: number; bytes: number }>('/api/trash');

/** Moves a batch back to its original locations; 409 if one is occupied. */
export const restoreTrash = (id: string) => request<TrashBatch>(`/api/trash/${id}/restore`, { method: 'POST' });

/** Permanently deletes one batch, or with no id, empties the trash. */
export async function purgeTrash(id?: string) {
  const res = await fetch(id ? `/api/trash/${id}` : '/api/trash', { method: 'DELETE' });
  if (!res.ok) throw new ApiError(res.status, (await res.text()).trim(), 'DELETE', '/api/trash');
}

export const patchCollection = (id: number, body: Patch) => patch<Collection>(`/api/collections/${id}`, body);

export const rescan = () => request<ScanResult>('/api/rescan', { method: 'POST' });

const imageFormats = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp']);

/** Whether an indexed file is an image the browser can show. */
export const isImage = (m: Model) => m.kind === 'file' && imageFormats.has(m.format);

export const fileURL = (id: number) => `/api/models/${id}/file`;

export const meshURL = (id: number) => `/api/models/${id}/mesh.stl`;
