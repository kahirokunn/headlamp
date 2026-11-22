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

import * as yaml from 'js-yaml';
import { decodeBase64, encodeBase64 } from '../helpers/base64';
import type { KubeconfigObject } from '../lib/k8s/kubeconfig';

/** Name used for this context in the backend's parsed cluster list. */
export function effectiveContextName(context: KubeconfigObject['contexts'][number]) {
  const custom = context.context.extensions?.find(extension => extension.name === 'headlamp_info');
  const info = custom?.extension;
  // The backend skips a context whose headlamp_info cannot be read into its custom-name object.
  if (
    info !== undefined &&
    info !== null &&
    (typeof info !== 'object' ||
      Array.isArray(info) ||
      (info.customName !== undefined &&
        info.customName !== null &&
        typeof info.customName !== 'string'))
  )
    return undefined;
  return info?.customName || context.name.replaceAll('/', '--').replaceAll(' ', '__');
}

/** Names that can be selected from this saved record. */
export function storedContextNames(bytes: string) {
  const config = yaml.load(decodeBase64(bytes)) as KubeconfigObject;
  const unique = new Set<string>();
  const duplicates = new Set<string>();
  for (const context of config.contexts ?? []) {
    const name = effectiveContextName(context);
    if (name === undefined) continue;
    if (unique.has(name)) duplicates.add(name);
    unique.add(name);
  }
  for (const name of duplicates) unique.delete(name);
  return unique;
}

/**
 * Finds the kubeconfig and context with the matching cluster name or custom name in headlamp_info.
 * @param clusterID The ID for a cluster, composed of the kubeconfig path and cluster name
 * @param clusterName The name of the cluster to find.
 * @param parsedKubeconfig The parsed kubeconfig object.
 * @returns An object containing the matching kubeconfig and context.
 */
export function findMatchingContexts(
  clusterName: string,
  parsedKubeconfig: KubeconfigObject,
  clusterID?: string
) {
  let matchingContext;
  let matchingKubeconfig;

  // Note: currently clusterID is being used for non dynamic clusters only
  if (clusterID) {
    // Find source for the kubeconfig
    const source = parsedKubeconfig.contexts.find(
      context => context.context.clusterID === clusterID
    )?.context.source;

    // Find the context with the matching clusterID
    if (source === 'kubeconfig') {
      matchingKubeconfig = parsedKubeconfig.contexts.find(
        context => context.context.clusterID === clusterID
      );
    }
  } else {
    matchingContext = parsedKubeconfig.contexts.find(
      context => effectiveContextName(context) === clusterName
    );
    if (matchingContext?.name === clusterName) matchingKubeconfig = matchingContext;
  }

  return { matchingKubeconfig, matchingContext };
}

/** Returns the remaining kubeconfig bytes, or null when the last context is removed. */
export function withoutKubeconfigContexts(
  bytes: string,
  names: string[],
  clusterID?: string
): string | null {
  const config = yaml.load(decodeBase64(bytes)) as KubeconfigObject;
  const removed = new Set(
    config.contexts
      .filter(context => {
        if (clusterID)
          return context.context.clusterID === clusterID && context.context.source === 'kubeconfig';
        const name = effectiveContextName(context);
        return name !== undefined && names.includes(name);
      })
      .map(context => context.name)
  );
  if (!removed.size) return bytes;
  config.contexts = config.contexts.filter(context => !removed.has(context.name));
  if (!config.contexts.length) return null;
  if (removed.has(config['current-context'])) config['current-context'] = config.contexts[0].name;
  const clusters = new Set(config.contexts.map(item => item.context.cluster));
  const users = new Set(config.contexts.map(item => item.context.user));
  if (config.clusters) config.clusters = config.clusters.filter(item => clusters.has(item.name));
  if (config.users) config.users = config.users.filter(item => users.has(item.name));
  return encodeBase64(yaml.dump(config));
}
