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

import nock from 'nock';
import { afterEach, beforeEach, describe, expect, it, Mock, vi } from 'vitest';
import { setBackendToken } from '../../../../helpers/getHeadlampAPIHeaders';
import { findKubeconfigByClusterName } from '../../../../stateless/findKubeconfigByClusterName';
import { getUserIdFromLocalStorage } from '../../../../stateless/getUserIdFromLocalStorage';
import * as importKey from '../../../../stateless/importRevisionKey';
import { getClusterAuthType } from '../../clusterAuthType';
import * as discoveredRevisions from './discoveredConnectionRevisions';
import { BASE_HTTP_URL, clusterFetch } from './fetch';

vi.mock('../../../auth', () => ({
  getToken: vi.fn(),
  setToken: vi.fn(),
}));

vi.mock('../../../../stateless/findKubeconfigByClusterName', () => ({
  findKubeconfigByClusterName: vi.fn(),
}));

vi.mock('../../../../stateless/getUserIdFromLocalStorage', () => ({
  getUserIdFromLocalStorage: vi.fn(),
}));

vi.mock('../../clusterAuthType', () => ({
  getClusterAuthType: vi.fn(),
}));

vi.mock('../v1/tokenApi', () => ({
  refreshToken: vi.fn(),
}));

describe('clusterFetch', () => {
  const clusterName = 'test-cluster';
  const testUrl = '/test/url';
  const mockResponse = { message: 'mock response' };
  const kubeconfig = 'mock-kubeconfig';
  const userID = 'mock-user-id';

  beforeEach(() => {
    vi.resetAllMocks();
    setBackendToken('desktop-token');
    (findKubeconfigByClusterName as Mock).mockResolvedValue(kubeconfig);
    (getUserIdFromLocalStorage as Mock).mockReturnValue(userID);
    (getClusterAuthType as Mock).mockReturnValue('serviceAccount');
  });

  afterEach(() => {
    setBackendToken(null);
    nock.cleanAll();
  });

  it('Successfully makes a request', async () => {
    nock(BASE_HTTP_URL).get(`/clusters/${clusterName}${testUrl}`).reply(200, mockResponse);

    const response = await clusterFetch(testUrl, { cluster: clusterName });
    const responseBody = await response.json();

    expect(responseBody).toEqual(mockResponse);
  });

  it('checks the local connection again after looking up kubeconfig', async () => {
    const revision = vi
      .spyOn(discoveredRevisions, 'getDiscoveredConnectionRevision')
      .mockReturnValue('same:1');
    const lookup = Promise.withResolvers<string>();
    vi.mocked(findKubeconfigByClusterName).mockReturnValueOnce(lookup.promise);
    const request = nock(BASE_HTTP_URL).post(`/clusters/${clusterName}${testUrl}`).reply(200, {});
    try {
      const response = clusterFetch(testUrl, {
        cluster: clusterName,
        method: 'POST',
        expectedDiscoveryConnection: { revision: 'same:1' },
      });
      revision.mockReturnValue('same:2');
      lookup.resolve(kubeconfig);

      await expect(response).rejects.toMatchObject({ status: 409 });
      expect(request.isDone()).toBe(false);
    } finally {
      revision.mockRestore();
    }
  });

  it('does not send a write when the connection changes during import-key lookup', async () => {
    const lookup = Promise.withResolvers<string>();
    const started = Promise.withResolvers<void>();
    const key = vi.spyOn(importKey, 'getImportRevisionKey').mockImplementationOnce(() => {
      started.resolve();
      return lookup.promise;
    });
    const revision = vi
      .spyOn(discoveredRevisions, 'getDiscoveredConnectionRevision')
      .mockReturnValue('same:1');
    const upstream = nock(BASE_HTTP_URL).post(`/clusters/${clusterName}${testUrl}`).reply(200, {});
    try {
      const response = clusterFetch(testUrl, { cluster: clusterName, method: 'POST' });
      await started.promise;
      revision.mockReturnValue('same:2');
      lookup.resolve('key');
      await expect(response).rejects.toMatchObject({ status: 409 });
      expect(upstream.isDone()).toBe(false);
    } finally {
      key.mockRestore();
      revision.mockRestore();
    }
  });

  it('keeps an ordinary request bound while looking up its kubeconfig', async () => {
    const revision = vi
      .spyOn(discoveredRevisions, 'getDiscoveredConnectionRevision')
      .mockReturnValue(undefined);
    const lookup = Promise.withResolvers<null>();
    vi.mocked(findKubeconfigByClusterName).mockReturnValueOnce(lookup.promise);
    const upstream = nock(BASE_HTTP_URL).get(`/clusters/${clusterName}${testUrl}`).reply(200, {});
    try {
      const response = clusterFetch(testUrl, { cluster: clusterName });
      revision.mockReturnValue('replacement:1');
      lookup.resolve(null);
      await expect(response).rejects.toMatchObject({ status: 409 });
      expect(upstream.isDone()).toBe(false);
    } finally {
      revision.mockRestore();
    }
  });

  it('returns an accepted write when its connection is replaced during the request', async () => {
    const revision = vi
      .spyOn(discoveredRevisions, 'getDiscoveredConnectionRevision')
      .mockReturnValue(undefined);
    try {
      nock(BASE_HTTP_URL)
        .post(`/clusters/${clusterName}${testUrl}`)
        .reply(() => {
          revision.mockReturnValue('replacement:1');
          return [202, {}];
        });
      const result = await clusterFetch(testUrl, { cluster: clusterName, method: 'POST' });
      expect(result.status).toBe(202);
    } finally {
      revision.mockRestore();
    }
  });

  it('rejects a successful GET if its connection changed after dispatch', async () => {
    const revision = vi
      .spyOn(discoveredRevisions, 'getDiscoveredConnectionRevision')
      .mockReturnValue('first:1');
    try {
      nock(BASE_HTTP_URL)
        .get(`/clusters/${clusterName}${testUrl}`)
        .query(true)
        .reply(() => {
          revision.mockReturnValue('replacement:2');
          return [200, {}];
        });
      await expect(clusterFetch(testUrl, { cluster: clusterName })).rejects.toMatchObject({
        status: 409,
      });
    } finally {
      revision.mockRestore();
    }
  });

  it('identifies an ordinary connection to the backend', async () => {
    nock(BASE_HTTP_URL)
      .get(`/clusters/${clusterName}${testUrl}`)
      .matchHeader('X-Headlamp-Connection-Revision', 'unversioned')
      .reply(200, mockResponse);

    await expect(clusterFetch(testUrl, { cluster: clusterName })).resolves.toBeDefined();
  });

  it('does not add backend credentials to non-cluster requests', async () => {
    let backendTokenHeader: string | string[] | undefined;
    let importKeyHeader: string | string[] | undefined;
    nock(BASE_HTTP_URL)
      .get(testUrl)
      .reply(function () {
        importKeyHeader = this.req.headers['x-headlamp-import-key'];
        backendTokenHeader = this.req.headers['x-headlamp_backend-token'];
        return [200, mockResponse];
      });

    await clusterFetch(testUrl, { cluster: '' });

    expect(backendTokenHeader).toBeUndefined();
    expect(importKeyHeader).toBeUndefined();
  });

  it('Sets KUBECONFIG and X-HEADLAMP-USER-ID headers if kubeconfig exists', async () => {
    nock(BASE_HTTP_URL)
      .get(`/clusters/${clusterName}${testUrl}`)
      .matchHeader('KUBECONFIG', kubeconfig)
      .matchHeader('X-Headlamp-Import-Key', /^[0-9a-f]{64}$/)
      .matchHeader('X-HEADLAMP-USER-ID', userID)
      .matchHeader('X-HEADLAMP_BACKEND-TOKEN', 'desktop-token')
      .reply(200, mockResponse);

    await clusterFetch(testUrl, { cluster: clusterName });
  });

  it('preserves caller headers while adding the backend token', async () => {
    nock(BASE_HTTP_URL)
      .get(`/clusters/${clusterName}${testUrl}`)
      .matchHeader('X-CUSTOM-HEADER', 'caller-value')
      .matchHeader('X-HEADLAMP_BACKEND-TOKEN', 'desktop-token')
      .reply(200, mockResponse);

    await clusterFetch(testUrl, {
      cluster: clusterName,
      headers: { 'X-CUSTOM-HEADER': 'caller-value' },
    });
  });

  it('Throws an error if response is not ok', async () => {
    nock(BASE_HTTP_URL).get(`/clusters/${clusterName}${testUrl}`).reply(500);

    await expect(clusterFetch(testUrl, { cluster: clusterName })).rejects.toThrow('Unreachable');
  });

  it('extracts plain text error message from non-JSON response', async () => {
    nock(BASE_HTTP_URL)
      .get(`/clusters/${clusterName}${testUrl}`)
      .reply(400, 'Invalid cluster name\n');

    await expect(clusterFetch(testUrl, { cluster: clusterName })).rejects.toThrow(
      'Invalid cluster name'
    );
  });

  it('extracts error field from JSON error response when message is absent', async () => {
    nock(BASE_HTTP_URL)
      .get(`/clusters/${clusterName}${testUrl}`)
      .reply(400, { error: 'invalid_grant' });

    await expect(clusterFetch(testUrl, { cluster: clusterName })).rejects.toThrow('invalid_grant');
  });

  it('extracts error_description from OAuth-style error response', async () => {
    nock(BASE_HTTP_URL)
      .get(`/clusters/${clusterName}${testUrl}`)
      .reply(400, { error: 'invalid_request', error_description: 'Code parameter is missing' });

    await expect(clusterFetch(testUrl, { cluster: clusterName })).rejects.toThrow(
      'Code parameter is missing'
    );
  });
});
