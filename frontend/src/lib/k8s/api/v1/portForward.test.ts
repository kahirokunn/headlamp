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

import { findKubeconfigByClusterName } from '../../../../stateless/findKubeconfigByClusterName';
import { getDiscoveredConnectionRevision } from '../v2/discoveredConnectionRevisions';
import { clusterFetch } from '../v2/fetch';
import { listPortForward, startPortForward, stopOrDeletePortForward } from './portForward';

vi.mock('../../../../stateless/findKubeconfigByClusterName', () => ({
  findKubeconfigByClusterName: vi.fn(),
}));
vi.mock('../v2/discoveredConnectionRevisions', () => ({
  getDiscoveredConnectionRevision: vi.fn(),
}));
vi.mock('../v2/fetch', () => ({ clusterFetch: vi.fn() }));

beforeEach(() => {
  vi.resetAllMocks();
  vi.mocked(getDiscoveredConnectionRevision).mockReturnValue('original:1');
  vi.mocked(findKubeconfigByClusterName).mockResolvedValue(null);
});

it.each([false, true])('never sends an obsolete stop/delete request (stop=%s)', async stop => {
  const kubeconfig = Promise.withResolvers<string | null>();
  vi.mocked(findKubeconfigByClusterName).mockReturnValueOnce(kubeconfig.promise);
  vi.mocked(clusterFetch).mockResolvedValue(new Response('stopped'));

  const pending = stopOrDeletePortForward('workload', 'shared-id', stop);
  vi.mocked(getDiscoveredConnectionRevision).mockReturnValue('replacement:2');
  kubeconfig.resolve(null);

  await expect(pending).rejects.toMatchObject({ status: 409 });
  expect(clusterFetch).not.toHaveBeenCalled();
});

it.each([
  ['list', () => listPortForward('workload')],
  ['start', () => startPortForward('workload', 'default', 'pod', 80, '', '')],
])('ignores a delayed %s result after replacement', async (_operation, run) => {
  const response = Promise.withResolvers<Response>();
  const requested = Promise.withResolvers<void>();
  vi.mocked(clusterFetch).mockImplementationOnce(() => {
    requested.resolve();
    return response.promise;
  });

  const pending = run();
  await requested.promise;
  vi.mocked(getDiscoveredConnectionRevision).mockReturnValue('replacement:2');
  response.resolve(Response.json({ id: 'shared-id', connectionRevision: 'original' }));

  await expect(pending).rejects.toMatchObject({ status: 409 });
});
