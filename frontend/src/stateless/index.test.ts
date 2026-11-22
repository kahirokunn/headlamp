/*
 * Copyright 2025 The Kubernetes Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setStatelessConfig } from '../redux/configSlice';
import store from '../redux/stores/store';
import { deleteClusterKubeconfig } from './deleteClusterKubeconfig';
import { findKubeconfigByClusterName } from './findKubeconfigByClusterName';
import {
  fetchStatelessClusterKubeConfigs,
  findAndReplaceKubeconfig,
  getStatelessClusterKubeConfigs,
  mergeStatelessConfigState,
  storeStatelessClusterKubeconfig,
  toStatelessConfigState,
} from './index';
import { installKubeconfigRecord } from './installKubeconfigRecord';
import {
  discardPendingKubeconfigRecord,
  displacedImport,
  publishKubeconfigRecord,
  renewRetiredImport,
  type StoredKubeconfig,
} from './records';
import { findMatchingContexts, storedContextNames } from './storedContexts';
import { updateStatelessClusterKubeconfig } from './updateStatelessClusterKubeconfig';

vi.mock('../lib/k8s/api/v1/clusterRequests', async importOriginal => {
  const actual = (await importOriginal()) as any;
  return { ...actual, request: vi.fn() };
});
import { request } from '../lib/k8s/api/v1/clusterRequests';

it('rejects when IndexedDB cannot open the imported kubeconfigs', async () => {
  const request = {} as IDBOpenDBRequest;
  const error = new DOMException('Cannot open kubeconfigs', 'UnknownError');
  vi.stubGlobal('indexedDB', { open: vi.fn().mockReturnValue(request) });
  const log = vi.spyOn(console, 'error').mockImplementation(() => {});

  try {
    const lookup = findKubeconfigByClusterName('imported');
    request.onerror?.call(request, { target: { error } } as unknown as Event);
    await expect(lookup).rejects.toBe(error);
  } finally {
    vi.unstubAllGlobals();
    log.mockRestore();
  }
});

describe('findAndReplaceKubeconfig', () => {
  it('reloads the selected record when saved imports contain the same context name', async () => {
    const makeKubeconfig = (endpoint: string) =>
      btoa(`apiVersion: v1
kind: Config
contexts:
- name: shared-import
  context:
    cluster: workload
    user: local-user
clusters:
- name: workload
  cluster:
    server: ${endpoint}
users:
- name: local-user
  user:
    token: sample-token
`);
    const first = makeKubeconfig('https://original.example.com');
    const second = makeKubeconfig('https://replacement.example.com');
    await storeStatelessClusterKubeconfig(first);
    let replacementRecord: number | undefined;
    await storeStatelessClusterKubeconfig(second, id => {
      replacementRecord = id;
    });
    vi.mocked(request).mockResolvedValueOnce({
      clusters: [
        { name: 'shared-import', kubeconfigIndex: 0, importRevision: 'import-original' },
        { name: 'shared-import', kubeconfigIndex: 1, importRevision: 'import-replacement' },
      ],
    });
    const initial = store.getState().config.statelessClusters;
    try {
      await fetchStatelessClusterKubeConfigs(store.dispatch);
      expect(store.getState().config.statelessClusters?.['shared-import']).toMatchObject({
        kubeconfigRecordId: replacementRecord,
        importRevision: 'import-replacement',
      });
      await expect(findKubeconfigByClusterName('shared-import')).resolves.toBe(second);
    } finally {
      store.dispatch(setStatelessConfig({ statelessClusters: initial }));
    }
  });

  beforeEach(() => {
    store.dispatch(setStatelessConfig({ statelessClusters: null }));
    vi.mocked(request).mockReset();
    localStorage.clear();
  });

  afterEach(async () => {
    // Clear the object store instead of deleting the database
    return new Promise<void>(resolve => {
      const request = indexedDB.open('kubeconfigs', 1);
      request.onsuccess = event => {
        const db = (event.target as any).result;
        if (db) {
          const transaction = db.transaction(['kubeconfigStore'], 'readwrite');
          const store = transaction.objectStore('kubeconfigStore');
          const clearRequest = store.clear();
          clearRequest.onsuccess = () => {
            db.close();
            resolve();
          };
          clearRequest.onerror = () => {
            db.close();
            resolve();
          };
        } else {
          resolve();
        }
      };
      request.onerror = () => resolve();
    });
  });

  it.each(['explicit', 'selected'] as const)(
    'isolates updates and deletion by %s record ID',
    async mode => {
      const first = btoa(
        JSON.stringify({ contexts: [{ name: 'shared', context: { cluster: 'one' } }] })
      );
      const second = btoa(
        JSON.stringify({ contexts: [{ name: 'shared', context: { cluster: 'two' } }] })
      );
      const ids: number[] = [];
      for (const bytes of [first, second])
        await storeStatelessClusterKubeconfig(bytes, id => ids.push(id));
      const original = store.getState().config.statelessClusters;
      try {
        store.dispatch(
          setStatelessConfig({
            statelessClusters: {
              shared: {
                name: 'shared',
                auth_type: '',
                kubeconfigRecordId: mode === 'explicit' ? ids[0] : ids[1],
              },
            },
          })
        );
        const record = mode === 'explicit' ? ids[1] : undefined;
        await updateStatelessClusterKubeconfig(second, 'renamed', 'shared', record);
        expect(await findKubeconfigByClusterName('shared', undefined, ids[0])).toBe(first);
        expect(await findKubeconfigByClusterName('renamed', undefined, ids[1])).not.toBeNull();
        // A rename changes the connection name visible to callers.
        store.dispatch(
          setStatelessConfig({
            statelessClusters: {
              renamed: {
                name: 'renamed',
                auth_type: 'token',
                kubeconfigRecordId: mode === 'explicit' ? ids[0] : ids[1],
              },
            },
          })
        );
        await deleteClusterKubeconfig('renamed', undefined, record);
        expect(await deleteClusterKubeconfig('renamed', undefined, record)).toBeNull();
        expect(await findKubeconfigByClusterName('shared', undefined, ids[0])).toBe(first);
      } finally {
        store.dispatch(setStatelessConfig({ statelessClusters: original }));
      }
    }
  );

  it('preserves the unclaimed sibling when two staged imports overlap at publication', async () => {
    const bytes = (names: string[]) =>
      btoa(JSON.stringify({ contexts: names.map(name => ({ name, context: { cluster: name } })) }));
    const ids: number[] = [];
    for (const [index, names] of [['x', 'y'], ['x'], ['x', 'y']].entries()) {
      await storeStatelessClusterKubeconfig(bytes(names), id => ids.push(id), index !== 0);
    }
    const cluster = (name: string, id: number) => ({
      name,
      auth_type: 'token',
      kubeconfigRecordId: id,
    });
    const original = { x: cluster('x', ids[0]), y: cluster('y', ids[0]) };
    store.dispatch(setStatelessConfig({ statelessClusters: original }));
    const first = installKubeconfigRecord(ids[1], [original.x], ['x'], () => ({
      statelessClusters: { ...original, x: cluster('x', ids[1]) },
    }));
    const second = installKubeconfigRecord(ids[2], [original.x, original.y], ['x', 'y'], () => {
      // A second browser tab still sees its original Redux selection.
      return { statelessClusters: { x: cluster('x', ids[2]), y: cluster('y', ids[2]) } };
    });
    const results = await Promise.allSettled([first, second]);
    expect(results.map(result => result.status)).toEqual(['fulfilled', 'rejected']);
    expect(await findKubeconfigByClusterName('x')).not.toBeNull();
    expect(await findKubeconfigByClusterName('y')).not.toBeNull();
    expect(store.getState().config.statelessClusters?.y.kubeconfigRecordId).toBe(ids[0]);
  });

  it('renames the newest committed import before Redux has selected a record', async () => {
    const bytes = btoa(
      JSON.stringify({ contexts: [{ name: 'shared', context: { cluster: 'c' } }] })
    );
    const ids: number[] = [];
    for (const pending of [false, false, true]) {
      await storeStatelessClusterKubeconfig(bytes, id => ids.push(id), pending);
    }
    store.dispatch(setStatelessConfig({ statelessClusters: {} }));
    await updateStatelessClusterKubeconfig(bytes, 'renamed', 'shared');
    expect(await findKubeconfigByClusterName('shared', undefined, ids[0])).toBe(bytes);
    expect(await findKubeconfigByClusterName('renamed', undefined, ids[1])).not.toBeNull();
  });

  it('keeps an aborted replacement out of Redux subscribers', async () => {
    const ids: number[] = [];
    const valid = btoa(JSON.stringify({ contexts: [{ name: 'x', context: { cluster: 'x' } }] }));
    // Simulate a legacy record that fails to decode while the transaction prunes it.
    await storeStatelessClusterKubeconfig('?', id => ids.push(id));
    await storeStatelessClusterKubeconfig(valid, id => ids.push(id), true);
    const cluster = (id: number) => ({ name: 'x', auth_type: 'token', kubeconfigRecordId: id });
    const old = cluster(ids[0]);
    store.dispatch(setStatelessConfig({ statelessClusters: { x: old } }));
    const observed: number[] = [];
    const unsubscribe = store.subscribe(() => {
      const id = store.getState().config.statelessClusters?.x?.kubeconfigRecordId;
      if (id !== undefined) observed.push(id);
    });
    try {
      await expect(
        installKubeconfigRecord(ids[1], [old], ['x'], () => ({
          statelessClusters: { x: cluster(ids[1]) },
        }))
      ).rejects.toThrow();
      expect(observed).not.toContain(ids[1]);
      expect(store.getState().config.statelessClusters?.x?.kubeconfigRecordId).toBe(ids[0]);
    } finally {
      unsubscribe();
    }
  });

  it('selects the effective name when a sibling uses it as a raw context name', () => {
    const contexts = [
      {
        name: 'shared',
        context: { extensions: [{ name: 'headlamp_info', extension: { customName: 'other' } }] },
      },
      {
        name: 'old',
        context: { extensions: [{ name: 'headlamp_info', extension: { customName: 'shared' } }] },
      },
      { name: 'team/one two', context: {} },
    ];
    const config = { contexts } as Parameters<typeof findMatchingContexts>[1];
    expect(findMatchingContexts('shared', config).matchingContext?.name).toBe('old');
    expect(findMatchingContexts('other', config).matchingContext?.name).toBe('shared');
    expect(findMatchingContexts('team--one__two', config).matchingContext?.name).toBe(
      'team/one two'
    );
  });

  it('counts only usable context names when a malformed alias collides', () => {
    const contexts = [
      { name: 'team/prod', context: { extensions: [{ name: 'headlamp_info', extension: 'bad' }] } },
      { name: 'team--prod', context: {} },
    ];
    const bytes = btoa(JSON.stringify({ contexts }));
    expect([...storedContextNames(bytes)]).toEqual(['team--prod']);
    expect(findMatchingContexts('team--prod', JSON.parse(atob(bytes))).matchingContext?.name).toBe(
      'team--prod'
    );
    contexts[0].context = {};
    expect(storedContextNames(btoa(JSON.stringify({ contexts }))).size).toBe(0);
  });

  it('rejects a replacement captured before another tab renewed the same record', async () => {
    const bytes = btoa(JSON.stringify({ contexts: [{ name: 'shared', context: {} }] }));
    const ids: number[] = [];
    await storeStatelessClusterKubeconfig(bytes, id => ids.push(id));
    await storeStatelessClusterKubeconfig(bytes, id => ids.push(id), true);
    const previous = {
      name: 'shared',
      auth_type: 'token' as const,
      kubeconfigRecordId: ids[0],
      importRevision: `import-${'a'.repeat(32)}-${ids[0]}`,
    };
    const renewed = await renewRetiredImport(previous);

    await expect(
      publishKubeconfigRecord(ids[1], [displacedImport(previous)], undefined, ['shared'])
    ).rejects.toThrow('changed during import');
    expect(await findKubeconfigByClusterName('shared', undefined, ids[0])).toBe(bytes);
    await publishKubeconfigRecord(
      ids[1],
      [displacedImport({ ...previous, importRevision: renewed })],
      undefined,
      ['shared']
    );
    expect(await findKubeconfigByClusterName('shared', undefined, ids[1])).toBe(bytes);
  });

  it('publishes a replacement and removes only the displaced contexts from the old record', async () => {
    const config = (names: string[]) =>
      btoa(
        JSON.stringify({
          contexts: names.map(name => ({ name, context: { cluster: name, user: name } })),
          clusters: names.map(name => ({ name })),
          users: names.map(name => ({ name })),
          'current-context': names[0],
        })
      );
    const ids: number[] = [];
    await storeStatelessClusterKubeconfig(config(['alpha', 'beta', 'keep']), id => ids.push(id));
    await storeStatelessClusterKubeconfig(config(['alpha', 'beta']), id => ids.push(id), true);
    await publishKubeconfigRecord(
      ids[1],
      ['alpha', 'beta'].map(name => ({ name, kubeconfigRecordId: ids[0] }))
    );
    await discardPendingKubeconfigRecord(ids[1]);
    expect(await findKubeconfigByClusterName('alpha', undefined, ids[1])).not.toBeNull();
    await deleteClusterKubeconfig('alpha', undefined, ids[1]);
    expect(await findKubeconfigByClusterName('alpha')).toBeNull();
    expect(await findKubeconfigByClusterName('beta', undefined, ids[0])).toBeNull();
    expect(await findKubeconfigByClusterName('keep', undefined, ids[0])).not.toBeNull();
    expect(await findKubeconfigByClusterName('beta', undefined, ids[1])).not.toBeNull();
  });

  it('removes both aliases from an older import and keeps its unrelated context', async () => {
    const names = ['team/a', 'team--a', 'other'];
    await storeStatelessClusterKubeconfig(
      btoa(
        JSON.stringify({
          contexts: names.map(name => ({ name, context: { cluster: name } })),
        })
      )
    );
    await deleteClusterKubeconfig('team--a');
    expect(await findKubeconfigByClusterName('team--a')).toBeNull();
    expect(await findKubeconfigByClusterName('other')).not.toBeNull();
  });

  it('renews only the retired context in a shared kubeconfig record', async () => {
    const raw = btoa(
      JSON.stringify({
        contexts: [
          { name: 'alpha', context: {} },
          { name: 'beta', context: {} },
        ],
      })
    );
    let record: number | undefined;
    await storeStatelessClusterKubeconfig(raw, id => {
      record = id;
    });
    expect(record).toBeDefined();

    const parsed = {
      clusters: [
        { name: 'alpha', auth_type: 'token' as const, importRevision: `import-${'a'.repeat(32)}` },
        { name: 'beta', auth_type: 'token' as const, importRevision: `import-${'b'.repeat(32)}` },
      ],
    };
    const original = toStatelessConfigState(parsed, [record!]).statelessClusters;
    expect(original.alpha.importRevision).toBe(`import-${'a'.repeat(32)}-${record}`);
    const renewedRevision = await renewRetiredImport(original.alpha);
    expect(await renewRetiredImport(original.alpha)).toBe(renewedRevision);

    let stored: StoredKubeconfig | undefined;
    await findKubeconfigByClusterName('alpha', undefined, record, current => {
      stored = current;
    });
    const updated = toStatelessConfigState(
      parsed,
      [record!],
      [stored?.incarnations]
    ).statelessClusters;
    expect(updated.alpha.importRevision).not.toBe(original.alpha.importRevision);
    expect(updated.beta.importRevision).toBe(original.beta.importRevision);
  });

  it('should store first kubeconfig and verify it exists', async () => {
    const kubeconfig1 = btoa(`apiVersion: v1
kind: Config
clusters:
- name: test-cluster
  cluster:
    server: https://test-cluster.example.com
contexts:
- name: test-cluster
  context:
    cluster: test-cluster
    user: test-user
users:
- name: test-user
  user:
    token: test-token-1
extensions:
- name: headlamp_info
  extension:
    cluster_name: test-cluster`);

    await storeStatelessClusterKubeconfig(kubeconfig1);
    await expect(findKubeconfigByClusterName('test-cluster')).resolves.toBe(kubeconfig1);
    await expect(findKubeconfigByClusterName('nonexistent')).resolves.toBeNull();

    const configs = await getStatelessClusterKubeConfigs();
    expect(configs).toHaveLength(1);
    expect(configs[0]).toBe(kubeconfig1);
  });

  it('should store second kubeconfig and verify 2 exist', async () => {
    const kubeconfig1 = btoa(`apiVersion: v1
kind: Config
clusters:
- name: test-cluster
  cluster:
    server: https://test-cluster.example.com
contexts:
- name: test-cluster
  context:
    cluster: test-cluster
    user: test-user
users:
- name: test-user
  user:
    token: test-token-1
extensions:
- name: headlamp_info
  extension:
    cluster_name: test-cluster`);

    const kubeconfig2 = btoa(`apiVersion: v1
kind: Config
clusters:
- name: second-cluster
  cluster:
    server: https://second-cluster.example.com
contexts:
- name: second-cluster
  context:
    cluster: second-cluster
    user: second-user
users:
- name: second-user
  user:
    token: test-token-2
extensions:
- name: headlamp_info
  extension:
    cluster_name: second-cluster`);

    await storeStatelessClusterKubeconfig(kubeconfig1);
    await storeStatelessClusterKubeconfig(kubeconfig2);

    const configs = await getStatelessClusterKubeConfigs();
    expect(configs).toHaveLength(2);
    expect(configs).toContain(kubeconfig1);
    expect(configs).toContain(kubeconfig2);
  });

  it.each([
    { scenario: 'loaded import', loaded: true },
    { scenario: 'cold import', loaded: false },
    { scenario: 'identical cold import', loaded: false, identical: true },
    { scenario: 'competing selection', loaded: true, interrupt: true },
    { scenario: 'retirement succeeds after a lost response', loaded: true, retry: true },
    { scenario: 'retirement fails twice', loaded: true, failed: true },
    {
      scenario: 'retirement fails twice after another selection',
      loaded: true,
      interrupt: true,
      failed: true,
    },
  ])(
    'replaces the old import only after retirement finishes ($scenario)',
    async ({ loaded, interrupt, retry, failed, identical }) => {
      const kubeconfig1 = btoa(`apiVersion: v1
kind: Config
clusters:
- name: test-cluster
  cluster:
    server: https://test-cluster.example.com
contexts:
- name: test-cluster
  context:
    cluster: test-cluster
    user: test-user
users:
- name: test-user
  user:
    token: test-token-1
extensions:
- name: headlamp_info
  extension:
    cluster_name: test-cluster`);

      const kubeconfig2 = btoa(`apiVersion: v1
kind: Config
clusters:
- name: second-cluster
  cluster:
    server: https://second-cluster.example.com
contexts:
- name: second-cluster
  context:
    cluster: second-cluster
    user: second-user
users:
- name: second-user
  user:
    token: test-token-2
extensions:
- name: headlamp_info
  extension:
    cluster_name: second-cluster`);

      const replacementKubeconfig = identical
        ? kubeconfig1
        : btoa(`apiVersion: v1
kind: Config
clusters:
- name: test-cluster
  cluster:
    server: https://test-cluster-replaced.example.com
contexts:
- name: test-cluster
  context:
    cluster: test-cluster
    user: test-user
users:
- name: test-user
  user:
    token: test-token-replaced
extensions:
- name: headlamp_info
  extension:
    cluster_name: test-cluster`);

      // Store initial kubeconfigs.
      let oldRecord: number | undefined;
      await storeStatelessClusterKubeconfig(kubeconfig1, id => {
        oldRecord = id;
      });
      await storeStatelessClusterKubeconfig(kubeconfig2);
      const fingerprint = 'import-0123456789abcdef0123456789abcdef';
      const oldRevision = failed ? `${fingerprint}-${oldRecord}` : 'import-original';
      if (loaded) {
        store.dispatch(
          setStatelessConfig({
            statelessClusters: {
              'test-cluster': {
                name: 'test-cluster',
                auth_type: 'token',
                kubeconfigRecordId: oldRecord,
                importRevision: oldRevision,
              },
            },
          })
        );
      }
      vi.mocked(request).mockImplementationOnce(async (_path, params) => {
        const { kubeconfigs } = JSON.parse(String(params?.body)) as { kubeconfigs: string[] };
        return {
          clusters: kubeconfigs.map((raw, kubeconfigIndex) => ({
            name: 'test-cluster',
            auth_type: 'token',
            kubeconfigIndex,
            importRevision: raw === kubeconfig1 ? 'import-original' : 'import-replacement',
          })),
        };
      });

      // Verify initial state
      let configs = await getStatelessClusterKubeConfigs();
      expect(configs).toHaveLength(2);

      const retirement = Promise.withResolvers<void>();
      const retiring = Promise.withResolvers<void>();
      if (retry || failed) {
        vi.mocked(request).mockRejectedValueOnce(
          Object.assign(new Error('Lost response'), { status: 502 })
        );
      }
      vi.mocked(request).mockImplementationOnce(() => {
        retiring.resolve();
        return retirement.promise;
      });
      const replacement = findAndReplaceKubeconfig('test-cluster', replacementKubeconfig);
      await Promise.race([retiring.promise, replacement]);
      expect(store.getState().config.statelessClusters?.['test-cluster']?.kubeconfigRecordId).toBe(
        loaded ? oldRecord : undefined
      );
      expect(request).toHaveBeenLastCalledWith(
        '/stateless-connections/dGVzdC1jbHVzdGVy/retire',
        expect.objectContaining({
          method: 'POST',
          headers: expect.objectContaining({
            'X-HEADLAMP-USER-ID': expect.stringMatching(new RegExp(`--${oldRevision}$`)),
          }),
        }),
        false,
        false
      );
      expect(await getStatelessClusterKubeConfigs()).toEqual([kubeconfig1, kubeconfig2]);
      if (interrupt) {
        store.dispatch(
          setStatelessConfig({
            statelessClusters: {
              'test-cluster': {
                name: 'test-cluster',
                auth_type: 'token',
                kubeconfigRecordId: 9999,
              },
            },
          })
        );
      }
      if (failed) {
        retirement.reject(
          Object.assign(new Error('Retirement response was lost'), { status: 502 })
        );
        await expect(replacement).rejects.toThrow('Retirement response was lost');
        expect(await getStatelessClusterKubeConfigs()).toEqual([kubeconfig1, kubeconfig2]);
        let generation: string | undefined;
        await expect(
          findKubeconfigByClusterName('test-cluster', undefined, oldRecord, record => {
            generation = record.incarnations?.['test-cluster'];
          })
        ).resolves.toBe(kubeconfig1);
        expect(generation).toMatch(/^[0-9a-f]{16}$/);
        const active = store.getState().config.statelessClusters?.['test-cluster'];
        if (interrupt) {
          expect(active?.kubeconfigRecordId).toBe(9999);
          expect(active?.importRevision).toBeUndefined();
        } else {
          expect(active).toMatchObject({
            kubeconfigRecordId: oldRecord,
            importRevision: `${oldRevision}-${generation}`,
          });
        }
        return;
      }
      retirement.resolve();
      if (interrupt) {
        await expect(replacement).rejects.toMatchObject({ status: 409 });
        const records = await getStatelessClusterKubeConfigs();
        expect(records).toContain(kubeconfig1);
        expect(records).not.toContain(replacementKubeconfig);
        return;
      }
      await replacement;

      const selected = store.getState().config.statelessClusters?.['test-cluster'];
      expect(selected?.importRevision).toBe(identical ? 'import-original' : 'import-replacement');
      expect(selected?.kubeconfigRecordId).not.toBe(oldRecord);
      await expect(findKubeconfigByClusterName('test-cluster')).resolves.toBe(
        replacementKubeconfig
      );

      // Verify replacement
      configs = await getStatelessClusterKubeConfigs();
      expect(configs).toHaveLength(2);
      expect(configs).toContain(replacementKubeconfig);
      expect(configs).toContain(kubeconfig2);
    }
  );

  it('should create new kubeconfig when create=true', async () => {
    const kubeconfig1 = btoa(`apiVersion: v1
kind: Config
clusters:
- name: test-cluster
  cluster:
    server: https://test-cluster.example.com
contexts:
- name: test-cluster
  context:
    cluster: test-cluster
    user: test-user
users:
- name: test-user
  user:
    token: test-token-1
extensions:
- name: headlamp_info
  extension:
    cluster_name: test-cluster`);

    const kubeconfig2 = btoa(`apiVersion: v1
kind: Config
clusters:
- name: second-cluster
  cluster:
    server: https://second-cluster.example.com
contexts:
- name: second-cluster
  context:
    cluster: second-cluster
    user: second-user
users:
- name: second-user
  user:
    token: test-token-2
extensions:
- name: headlamp_info
  extension:
    cluster_name: second-cluster`);

    const kubeconfig3 = btoa(`apiVersion: v1
kind: Config
clusters:
- name: new-cluster
  cluster:
    server: https://new-cluster.example.com
contexts:
- name: new-cluster
  context:
    cluster: new-cluster
    user: new-user
users:
- name: new-user
  user:
    token: test-token-3
extensions:
- name: headlamp_info
  extension:
    cluster_name: new-cluster`);

    // Store initial kubeconfigs
    await storeStatelessClusterKubeconfig(kubeconfig1);
    await storeStatelessClusterKubeconfig(kubeconfig2);

    // Verify initial state
    let configs = await getStatelessClusterKubeConfigs();
    expect(configs).toHaveLength(2);

    // Add third kubeconfig with create=true
    vi.mocked(request).mockResolvedValueOnce({
      clusters: [{ name: 'new-cluster', auth_type: 'token', kubeconfigIndex: 0 }],
    });
    await findAndReplaceKubeconfig('new-cluster', kubeconfig3, true);

    // Verify 3 exist
    configs = await getStatelessClusterKubeConfigs();
    expect(configs).toHaveLength(3);
    expect(configs).toContain(kubeconfig1);
    expect(configs).toContain(kubeconfig2);
    expect(configs).toContain(kubeconfig3);
  });
});

describe('fetchStatelessClusterKubeConfigs', () => {
  const clearIndexedDB = () =>
    new Promise<void>(resolve => {
      const request = indexedDB.open('kubeconfigs', 1);
      request.onupgradeneeded = (event: any) => {
        const db = event.target.result as IDBDatabase;
        if (!db.objectStoreNames.contains('kubeconfigStore')) {
          db.createObjectStore('kubeconfigStore', { keyPath: 'id', autoIncrement: true });
        }
      };
      request.onsuccess = (event: any) => {
        const db = event.target.result as IDBDatabase;
        const tx = db.transaction(['kubeconfigStore'], 'readwrite');
        const clearReq = tx.objectStore('kubeconfigStore').clear();

        tx.oncomplete = () => {
          db.close();
          resolve();
        };

        tx.onerror = () => {
          db.close();
          resolve();
        };

        tx.onabort = () => {
          db.close();
          resolve();
        };

        clearReq.onerror = () => {
          // The transaction handlers above finalize the promise.
        };
      };
      request.onerror = () => resolve();
    });

  beforeEach(async () => {
    await clearIndexedDB();
    store.dispatch(setStatelessConfig({ statelessClusters: null }));
  });

  afterEach(async () => {
    await clearIndexedDB();
    store.dispatch(setStatelessConfig({ statelessClusters: null }));
  });

  it('dispatches setStatelessConfig({}) when IndexedDB is empty and stale state exists', async () => {
    // Seed stale redux state (simulates a previous session leaving clusters behind)
    store.dispatch(
      setStatelessConfig({
        statelessClusters: { 'stale-cluster': { name: 'stale-cluster' } as any },
      })
    );
    expect(store.getState().config.statelessClusters).not.toBeNull();

    const dispatch = vi.fn();
    // IndexedDB is empty — fetchStatelessClusterKubeConfigs should clear stale state
    await fetchStatelessClusterKubeConfigs(dispatch);

    expect(dispatch).toHaveBeenCalledWith(setStatelessConfig({ statelessClusters: {} }));
  });

  it('does not dispatch when IndexedDB is empty and state is already clear', async () => {
    // statelessClusters is null in the store (already clean)
    expect(store.getState().config.statelessClusters).toBeNull();

    const dispatch = vi.fn();
    await fetchStatelessClusterKubeConfigs(dispatch);

    expect(dispatch).not.toHaveBeenCalled();
  });
});

describe('toStatelessConfigState', () => {
  it('maps parsed clusters by name for redux state', () => {
    const parsedConfig = {
      clusters: [
        { name: 'alpha', meta_data: {} },
        { name: 'beta', meta_data: {} },
      ],
    } as any;

    const nextState = toStatelessConfigState(parsedConfig);

    expect(Object.keys(nextState.statelessClusters)).toEqual(['alpha', 'beta']);
    expect(nextState.statelessClusters.alpha.name).toBe('alpha');
    expect(nextState.statelessClusters.beta.name).toBe('beta');
  });

  it('returns an empty statelessClusters map for missing clusters', () => {
    expect(toStatelessConfigState(undefined)).toEqual({ statelessClusters: {} });
    expect(toStatelessConfigState({} as any)).toEqual({ statelessClusters: {} });
  });
});

describe('mergeStatelessConfigState', () => {
  it('preserves existing clusters while adding parsed clusters', () => {
    const currentState = {
      alpha: { name: 'alpha', meta_data: {} },
    } as any;

    const parsedConfig = {
      clusters: [{ name: 'beta', meta_data: {} }],
    } as any;

    const nextState = mergeStatelessConfigState(currentState, parsedConfig);

    expect(Object.keys(nextState.statelessClusters)).toEqual(['alpha', 'beta']);
  });

  it('updates existing cluster entries when parsed data has same key', () => {
    const currentState = {
      alpha: { name: 'alpha', meta_data: { old: true } },
    } as any;

    const parsedConfig = {
      clusters: [{ name: 'alpha', meta_data: { old: false, fresh: true } }],
    } as any;

    const nextState = mergeStatelessConfigState(currentState, parsedConfig);

    expect(nextState.statelessClusters.alpha.meta_data).toEqual({ old: false, fresh: true });
  });

  it('does not drop pre-existing clusters when a renamed cluster is re-parsed', () => {
    // Simulates the ClusterNameEditor bug: after renaming "alpha", parseKubeConfig
    // returns only the renamed cluster's config. Without merging, all other stateless
    // clusters are wiped from the UI until the next periodic refresh.
    const sessionClusters = {
      alpha: { name: 'alpha', meta_data: {} },
      beta: { name: 'beta', meta_data: {} },
      gamma: { name: 'gamma', meta_data: {} },
    } as any;

    // Only "alpha" (now renamed to "alpha-new") comes back from the rename parse
    const parsedConfig = {
      clusters: [{ name: 'alpha-new', meta_data: {} }],
    } as any;

    const nextState = mergeStatelessConfigState(sessionClusters, parsedConfig);

    // All three original clusters must still be present
    expect(nextState.statelessClusters).toHaveProperty('beta');
    expect(nextState.statelessClusters).toHaveProperty('gamma');
    // The newly renamed cluster should also be present
    expect(nextState.statelessClusters).toHaveProperty('alpha-new');
  });
});

describe('fetchStatelessClusterKubeConfigs corner cases', () => {
  const clearIndexedDB = () =>
    new Promise<void>(resolve => {
      const req = indexedDB.open('kubeconfigs', 1);
      req.onupgradeneeded = (event: any) => {
        const db = event.target.result as IDBDatabase;
        if (!db.objectStoreNames.contains('kubeconfigStore')) {
          db.createObjectStore('kubeconfigStore', { keyPath: 'id', autoIncrement: true });
        }
      };
      req.onsuccess = (event: any) => {
        const db = event.target.result as IDBDatabase;
        const tx = db.transaction(['kubeconfigStore'], 'readwrite');
        tx.objectStore('kubeconfigStore').clear();
        tx.oncomplete = () => {
          db.close();
          resolve();
        };
        tx.onerror = () => {
          db.close();
          resolve();
        };
        tx.onabort = () => {
          db.close();
          resolve();
        };
      };
      req.onerror = () => resolve();
    });

  beforeEach(async () => {
    await clearIndexedDB();
    store.dispatch(setStatelessConfig({ statelessClusters: null }));
    vi.clearAllMocks();
  });

  afterEach(async () => {
    await clearIndexedDB();
    store.dispatch(setStatelessConfig({ statelessClusters: null }));
  });

  it.each(['another edit', 'another retirement fails'] as const)(
    'finishes a retired selection when %s prevents replacing the whole map',
    async interruption => {
      const alpha = {
        name: 'alpha',
        auth_type: 'token',
        importRevision: 'import-alpha',
        kubeconfigRecordId: 1,
      };
      const beta = {
        name: 'beta',
        auth_type: 'token',
        importRevision: 'import-beta',
        kubeconfigRecordId: 2,
      };
      store.dispatch(setStatelessConfig({ statelessClusters: { alpha, beta } }));
      const started = Promise.withResolvers<void>();
      const retiring = Promise.withResolvers<void>();
      vi.mocked(request).mockImplementation(async path => {
        if (path.includes('YWxwaGE')) {
          started.resolve();
          await retiring.promise;
        } else if (interruption === 'another retirement fails') {
          throw Object.assign(new Error('retirement rejected'), { status: 403 });
        }
      });
      const refresh = fetchStatelessClusterKubeConfigs(store.dispatch);
      await started.promise;
      if (interruption === 'another edit') {
        store.dispatch(
          setStatelessConfig({
            statelessClusters: { alpha, beta: { ...beta, importRevision: 'replacement' } },
          })
        );
      }
      retiring.resolve();
      await refresh;
      const visible = store.getState().config.statelessClusters;
      expect(visible?.alpha).toBeUndefined();
      expect(visible?.beta?.importRevision).toBe(
        interruption === 'another edit' ? 'replacement' : beta.importRevision
      );
    }
  );

  it('dispatches update when cluster names change even if count is the same', async () => {
    await storeStatelessClusterKubeconfig('dummy-kubeconfig');

    store.dispatch(
      setStatelessConfig({
        statelessClusters: {
          alpha: { name: 'alpha', meta_data: {} } as any,
          beta: { name: 'beta', meta_data: {} } as any,
        },
      })
    );

    vi.mocked(request).mockResolvedValue({
      clusters: [
        { name: 'alpha', meta_data: {} },
        { name: 'gamma', meta_data: {} }, // 'beta' replaced by 'gamma'
      ],
    });

    const dispatch = vi.fn();
    await fetchStatelessClusterKubeConfigs(dispatch);

    expect(dispatch).toHaveBeenCalled();
  });

  it('keeps the selected import when an earlier parse resolves', async () => {
    await storeStatelessClusterKubeconfig('dummy-kubeconfig');

    // Initial state before request starts.
    store.dispatch(
      setStatelessConfig({
        statelessClusters: {
          alpha: { name: 'alpha', meta_data: {} } as any,
        },
      })
    );

    const parse = Promise.withResolvers<unknown>();
    const started = Promise.withResolvers<void>();
    vi.mocked(request).mockImplementationOnce(() => {
      started.resolve();
      return parse.promise;
    });

    const dispatch = vi.fn();
    const inFlight = fetchStatelessClusterKubeConfigs(dispatch);
    await started.promise;

    // State changes while /parseKubeConfig is still in-flight.
    // The decision must use this latest value, not a stale captured value.
    store.dispatch(
      setStatelessConfig({
        statelessClusters: {
          alpha: { name: 'alpha', meta_data: {} } as any,
          beta: { name: 'beta', meta_data: {} } as any,
        },
      })
    );

    parse.resolve({ clusters: [{ name: 'alpha', meta_data: {} }] });
    await inFlight;

    expect(dispatch).not.toHaveBeenCalled();
  });

  it('dispatches update when cluster payload changes but keys stay the same', async () => {
    await storeStatelessClusterKubeconfig('dummy-kubeconfig');

    store.dispatch(
      setStatelessConfig({
        statelessClusters: {
          alpha: {
            name: 'alpha',
            meta_data: { project: 'old' },
            cluster: { server: 'https://old.example.com' },
          } as any,
        },
      })
    );

    vi.mocked(request).mockResolvedValue({
      clusters: [
        {
          name: 'alpha',
          meta_data: { project: 'new' },
          cluster: { server: 'https://new.example.com' },
        },
      ],
    });

    const dispatch = vi.fn();
    await fetchStatelessClusterKubeConfigs(dispatch);

    // Same key set, but payload differs, so redux state must still update.
    expect(dispatch).toHaveBeenCalled();
  });

  it('does not resolve before parse request settles', async () => {
    await storeStatelessClusterKubeconfig('dummy-kubeconfig');

    const parse = Promise.withResolvers<unknown>();
    const started = Promise.withResolvers<void>();
    vi.mocked(request).mockImplementationOnce(() => {
      started.resolve();
      return parse.promise;
    });

    const dispatch = vi.fn();
    const settled = vi.fn();
    const inFlight = fetchStatelessClusterKubeConfigs(dispatch).then(settled);

    await started.promise;
    await new Promise(resolve => setTimeout(resolve, 0));
    expect(settled).not.toHaveBeenCalled();

    parse.resolve({ clusters: [] });

    await inFlight;
    expect(settled).toHaveBeenCalledOnce();
  });
});
