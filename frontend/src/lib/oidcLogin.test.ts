/*
 * Copyright 2026 The Kubernetes Authors
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

import { findKubeconfigByClusterName } from '../stateless/findKubeconfigByClusterName';
import { getUserIdFromLocalStorage } from '../stateless/getUserIdFromLocalStorage';
import { getDiscoveredConnectionRevision } from './k8s/api/v2/discoveredConnectionRevisions';
import { backendFetch } from './k8s/api/v2/fetch';
import { prepareOIDCLogin } from './oidcLogin';

vi.mock('../stateless/findKubeconfigByClusterName', () => ({
  findKubeconfigByClusterName: vi.fn(),
}));
vi.mock('../stateless/getUserIdFromLocalStorage', () => ({
  getUserIdFromLocalStorage: vi.fn(),
}));
vi.mock('./k8s/api/v2/discoveredConnectionRevisions', () => ({
  getDiscoveredConnectionRevision: vi.fn(),
}));
vi.mock('./k8s/api/v2/fetch', () => ({ backendFetch: vi.fn() }));

describe('prepareOIDCLogin', () => {
  const routing = 'import-0123456789abcdef0123456789abcdef-9';
  const revision = routing + ':1';
  const path = '/oidc?cluster=workload&popup=true&__headlamp_connection_revision=' + routing;
  const url = 'https://headlamp.example' + path;
  const providerURL = 'https://provider.example/authorize?state=state';

  beforeEach(() => {
    vi.resetAllMocks();
    vi.mocked(getDiscoveredConnectionRevision).mockReturnValue(revision);
    vi.mocked(findKubeconfigByClusterName).mockResolvedValue('kubeconfig');
    vi.mocked(getUserIdFromLocalStorage).mockReturnValue('browser--' + routing);
  });

  it('keeps private import data in the preparation request headers', async () => {
    vi.mocked(backendFetch).mockResolvedValue(Response.json({ redirectURL: providerURL }));

    await expect(prepareOIDCLogin(url, 'workload', revision)).resolves.toBe(providerURL);
    expect(backendFetch).toHaveBeenCalledExactlyOnceWith(path, {
      method: 'POST',
      redirect: 'error',
      headers: expect.objectContaining({
        KUBECONFIG: 'kubeconfig',
        'X-HEADLAMP-USER-ID': 'browser--' + routing,
      }),
    });
  });

  it('prepares the selected import from a packaged desktop file URL', async () => {
    vi.stubGlobal('location', new URL('file:///Applications/Headlamp/frontend/index.html'));
    try {
      vi.mocked(backendFetch).mockResolvedValue(Response.json({ redirectURL: providerURL }));
      await expect(prepareOIDCLogin(url, 'workload', revision)).resolves.toBe(providerURL);
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('rejects a provider redirect received after the selected connection changes', async () => {
    const request = Promise.withResolvers<Response>();
    const arrived = Promise.withResolvers<void>();
    vi.mocked(backendFetch).mockImplementationOnce(() => {
      arrived.resolve();
      return request.promise;
    });
    const target = prepareOIDCLogin(url, 'workload', revision);
    await arrived.promise;
    vi.mocked(getDiscoveredConnectionRevision).mockReturnValue('replacement:2');
    request.resolve(Response.json({ redirectURL: providerURL }));

    await expect(target).rejects.toMatchObject({ status: 409 });
  });
});
