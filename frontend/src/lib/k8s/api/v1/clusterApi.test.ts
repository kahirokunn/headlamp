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

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { addBackstageAuthHeaders } from '../../../../helpers/addBackstageAuthHeaders';
import { getHeadlampAPIHeaders } from '../../../../helpers/getHeadlampAPIHeaders';
import { setStatelessConfig } from '../../../../redux/configSlice';
import store from '../../../../redux/stores/store';
import { storeStatelessClusterKubeconfig } from '../../../../stateless';
import { findKubeconfigByClusterName } from '../../../../stateless/findKubeconfigByClusterName';
import { getUserIdFromLocalStorage } from '../../../../stateless/getUserIdFromLocalStorage';
import * as rollback from '../../../../stateless/rollbackKubeconfigImport';
import * as discoveredConnections from '../v2/discoveredConnectionRevisions';
import { getClusterUserInfo, renameCluster, setCluster, testAuth } from './clusterApi';
import { clusterRequest, post, request } from './clusterRequests';

vi.mock('../../../../helpers/addBackstageAuthHeaders', () => ({
  addBackstageAuthHeaders: vi.fn((headers: Record<string, string>) => headers),
}));

vi.mock('../../../../helpers/getHeadlampAPIHeaders', () => ({
  getHeadlampAPIHeaders: vi.fn(() => ({
    'X-HEADLAMP_BACKEND-TOKEN': 'backend-token',
  })),
}));

vi.mock('../../../../stateless', () => ({
  storeStatelessClusterKubeconfig: vi.fn(),
}));

vi.mock('../../../../stateless/deleteClusterKubeconfig', () => ({
  deleteClusterKubeconfig: vi.fn(),
}));

vi.mock('../../../../stateless/findKubeconfigByClusterName', () => ({
  findKubeconfigByClusterName: vi.fn(),
}));

vi.mock('../../../../stateless/getUserIdFromLocalStorage', () => ({
  getUserIdFromLocalStorage: vi.fn(() => 'browser-user'),
}));

vi.mock('../../../cluster', () => ({
  getCluster: vi.fn(),
  getSelectedClusters: vi.fn(() => []),
}));

vi.mock('./clusterRequests', () => ({
  clusterRequest: vi.fn(),
  post: vi.fn(),
  request: vi.fn(),
}));

it('rejects a failed SelfSubjectReview from a replaced connection', async () => {
  const revision = vi
    .spyOn(discoveredConnections, 'getDiscoveredConnectionRevision')
    .mockReturnValue('original:1');
  const access = Promise.withResolvers<void>();
  const started = Promise.withResolvers<void>();
  vi.mocked(clusterRequest).mockResolvedValueOnce({});
  vi.mocked(post).mockImplementationOnce(() => {
    started.resolve();
    return access.promise;
  });
  try {
    const user = getClusterUserInfo('workload');
    await started.promise;
    revision.mockReturnValue('replacement:2');
    access.reject(new Error('lookup interrupted'));
    await expect(user).rejects.toMatchObject({ status: 409 });
  } finally {
    revision.mockRestore();
  }
});

it('keeps the user-info fallback on the connection selected before /me', async () => {
  const revision = vi
    .spyOn(discoveredConnections, 'getDiscoveredConnectionRevision')
    .mockReturnValue('original:1');
  const me = Promise.withResolvers<unknown>();
  vi.mocked(clusterRequest).mockReturnValueOnce(me.promise);
  vi.mocked(post)
    .mockClear()
    .mockResolvedValue({ status: { userInfo: { username: 'replacement-user' } } });

  try {
    const result = getClusterUserInfo('workload');
    revision.mockReturnValue('replacement:2');
    me.reject(new Error('/me failed'));

    await expect(result).rejects.toMatchObject({ status: 409 });
    expect(post).not.toHaveBeenCalled();
  } finally {
    revision.mockRestore();
  }
});

it('rejects authentication that succeeded on a superseded connection', async () => {
  const revision = vi
    .spyOn(discoveredConnections, 'getDiscoveredConnectionRevision')
    .mockReturnValue('original:1');
  const response = Promise.withResolvers<unknown>();
  vi.mocked(post).mockReturnValueOnce(response.promise);

  try {
    const authentication = testAuth('workload');
    revision.mockReturnValue('replacement:2');
    response.resolve({ status: { resourceRules: [] } });

    await expect(authentication).rejects.toMatchObject({ status: 409 });
  } finally {
    revision.mockRestore();
  }
});

describe('setCluster', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('includes backend auth headers for stateless parseKubeConfig request', async () => {
    const kubeconfig = 'base64-kubeconfig';
    const requestMock = vi.mocked(request);

    requestMock.mockResolvedValue({ clusters: [] });

    await setCluster({ kubeconfig });

    expect(storeStatelessClusterKubeconfig).toHaveBeenCalledWith(kubeconfig, expect.any(Function));
    expect(addBackstageAuthHeaders).toHaveBeenCalled();
    expect(getHeadlampAPIHeaders).toHaveBeenCalled();
    expect(requestMock).toHaveBeenCalledWith(
      '/parseKubeConfig',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ kubeconfigs: [kubeconfig] }),
        headers: expect.objectContaining({
          'X-HEADLAMP_BACKEND-TOKEN': 'backend-token',
        }),
      }),
      false,
      false
    );
  });
});

describe('renameCluster', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(request).mockImplementation(async () => Response.json({ clusters: [] }));
  });

  it('identifies the browser whose cached IndexedDB connection must be removed', async () => {
    vi.mocked(findKubeconfigByClusterName).mockResolvedValue('base64-kubeconfig');

    await renameCluster('shared-connection', 'my-workload', 'dynamic_cluster');

    expect(request).toHaveBeenCalledWith(
      '/cluster/shared-connection',
      expect.objectContaining({
        method: 'PUT',
        headers: expect.objectContaining({
          'X-HEADLAMP_BACKEND-TOKEN': 'backend-token',
          'X-HEADLAMP-USER-ID': 'browser-user',
        }),
        body: JSON.stringify({
          newClusterName: 'my-workload',
          source: 'dynamic_cluster',
          stateless: true,
        }),
      }),
      false,
      false
    );
  });

  it('recovers the import after the server accepts a rename but its response body fails', async () => {
    vi.mocked(findKubeconfigByClusterName).mockResolvedValue('base64-kubeconfig');
    vi.mocked(request).mockRejectedValueOnce(new SyntaxError('Incomplete rename response'));
    const original = store.getState().config.statelessClusters;
    const selected = {
      name: 'shared-connection',
      auth_type: 'token',
      kubeconfigRecordId: 41,
      importRevision: 'import-original',
    };
    store.dispatch(setStatelessConfig({ statelessClusters: { [selected.name]: selected } }));
    const recover = vi.spyOn(rollback, 'rollbackKubeconfigImport').mockResolvedValue(undefined);
    try {
      await expect(
        renameCluster('shared-connection', 'new-name', 'dynamic_cluster')
      ).rejects.toThrow();
      expect(recover).toHaveBeenCalledWith(undefined, [selected]);
    } finally {
      store.dispatch(setStatelessConfig({ statelessClusters: original }));
      recover.mockRestore();
    }
  });

  it('keeps file-backed renames independent of the browser identity', async () => {
    vi.mocked(findKubeconfigByClusterName).mockResolvedValue(null);

    await renameCluster('file-context', 'my-workload', 'kubeconfig');

    const [, options] = vi.mocked(request).mock.calls[0];
    expect(new Headers(options?.headers).has('X-HEADLAMP-USER-ID')).toBe(false);
    expect(JSON.parse(options!.body as string)).toMatchObject({ stateless: false });
    expect(getUserIdFromLocalStorage).not.toHaveBeenCalled();
  });
});
