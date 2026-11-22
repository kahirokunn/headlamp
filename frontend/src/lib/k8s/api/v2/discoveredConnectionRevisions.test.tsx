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

import { act, renderHook } from '@testing-library/react';
import { setConfig } from '../../../../redux/configSlice';
import store from '../../../../redux/stores/store';
import {
  getDiscoveredConnectionRevision,
  useDiscoveredConnectionRevisions,
} from './discoveredConnectionRevisions';
import { assertCurrentDiscoveryConnection } from './discoveryConnection';

it('bounds removed registrations and assigns a fresh revision when restored', () => {
  const saved = store.getState().config.clusters;
  const register = (name: string) =>
    store.dispatch(
      setConfig({
        clusters: {
          [name]: {
            name,
            server: 'https://api.example.test',
            auth_type: 'oidc',
            meta_data: {
              discoveryConnectionRevision: 'same-routing',
              discoveryOrigin: {
                cluster: 'hub',
                resource: {
                  apiVersion: 'fleet.example.test/v1',
                  kind: 'Cluster',
                  namespace: 'default',
                  name,
                  uid: name,
                },
              },
            },
          },
        },
      })
    );
  const removeAll = () => store.dispatch(setConfig({ clusters: {} }));
  const { result, unmount } = renderHook(useDiscoveredConnectionRevisions);

  try {
    act(() => register('restored'));
    const original = getDiscoveredConnectionRevision('restored');
    expect(original).toMatch(/^same-routing:/);
    for (let i = 0; i < 160; i++) {
      act(() => register(`transient-${i}`));
    }
    act(removeAll);

    expect(result.current.size).toBe(128);
    expect(result.current.has('restored')).toBe(false);
    expect(result.current.get('transient-159')).toBe('removed');
    expect(() => assertCurrentDiscoveryConnection('restored', original)).toThrow();

    act(() =>
      store.dispatch(
        setConfig({ clusters: { restored: { name: 'restored', auth_type: 'token' } } })
      )
    );
    expect(getDiscoveredConnectionRevision('restored')).toMatch(/^unversioned:/);
    expect(() => assertCurrentDiscoveryConnection('restored', undefined)).toThrow();

    act(() => register('restored'));
    expect(result.current.size).toBe(129);
    expect(result.current.get('restored')).toMatch(/^same-routing:/);
    expect(result.current.get('restored')).not.toBe(original);
    act(() =>
      store.dispatch(
        setConfig({ clusters: { restored: { name: 'restored', auth_type: 'token' } } })
      )
    );
    expect(getDiscoveredConnectionRevision('restored')).toMatch(/^unversioned:/);
    expect(() => assertCurrentDiscoveryConnection('restored', undefined)).toThrow();
  } finally {
    unmount();
    store.dispatch(setConfig({ clusters: saved }));
  }
});
