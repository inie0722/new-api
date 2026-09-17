/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { SettingsPageProvider } from '../components/settings-page-context'
import { PluginFileStorageSection } from '../general/plugin-file-storage-section'

const initialConfig = {
  mode: 's3',
  ttl_hours: 24,
  local_directory: './data/plugin-files',
  endpoint: 'https://storage.example.test',
  region: 'us-east-1',
  bucket: 'private-bucket',
  prefix: 'inputs',
  access_key: '',
  secret_key: '',
  path_style: true,
}

const clients: QueryClient[] = []
afterEach(() => {
  for (const client of clients.splice(0)) client.clear()
})

async function renderStorage(mode = 's3') {
  const settings = {
    config: { ...initialConfig, mode },
    public_address: 'https://files.example.test',
    access_key_configured: true,
    secret_key_configured: true,
  }
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: settings },
  })
  const put = vi.spyOn(api, 'put').mockImplementation(async (_url, data) => ({
    data: {
      success: true,
      data: {
        ...settings,
        config: {
          ...(data as typeof initialConfig),
          access_key: '',
          secret_key: '',
        },
      },
    },
  }))
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  const actions = document.createElement('div')
  document.body.append(actions)
  const router = createRouter({
    routeTree: createRootRoute({ component: PluginFileStorageSection }),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  await router.load()
  const result = render(
    <QueryClientProvider client={client}>
      <SettingsPageProvider actionsContainer={actions}>
        <RouterProvider router={router} />
      </SettingsPageProvider>
    </QueryClientProvider>
  )
  await screen.findByLabelText('File URL lifetime (hours)')
  return {
    put,
    settings,
    user: userEvent.setup(),
    cleanup: () => {
      result.unmount()
      actions.remove()
    },
  }
}

describe('plugin file storage settings', () => {
  test('saves the full configuration once and clears entered credentials after success', async () => {
    const fixture = await renderStorage()
    const secret = screen.getByLabelText('S3 secret key')
    expect(secret).toHaveValue('')
    expect(
      screen.getAllByText(
        'Configured. Leave blank to keep the current credential.'
      )
    ).toHaveLength(2)
    await fixture.user.type(secret, 'new-private-secret')
    await fixture.user.clear(screen.getByLabelText('File URL lifetime (hours)'))
    await fixture.user.type(
      screen.getByLabelText('File URL lifetime (hours)'),
      '48'
    )
    await fixture.user.click(
      screen.getByRole('button', { name: 'Save Changes' })
    )
    await waitFor(() => expect(fixture.put).toHaveBeenCalledTimes(1))
    expect(fixture.put).toHaveBeenCalledWith(
      '/api/option/plugin_file_storage',
      expect.objectContaining({
        ttl_hours: 48,
        bucket: 'private-bucket',
        secret_key: 'new-private-secret',
      })
    )
    await waitFor(() => expect(secret).toHaveValue(''))
    expect(screen.getByRole('button', { name: 'Reset' })).toBeDisabled()
    fixture.cleanup()
  })

  test('rejects an out-of-range lifetime without sending a settings request', async () => {
    const fixture = await renderStorage('local')
    expect(screen.getByLabelText('Public file address')).toHaveValue(
      'https://files.example.test'
    )
    expect(screen.queryByLabelText('S3 secret key')).not.toBeInTheDocument()
    await fixture.user.clear(screen.getByLabelText('File URL lifetime (hours)'))
    await fixture.user.type(
      screen.getByLabelText('File URL lifetime (hours)'),
      '169'
    )
    await fixture.user.click(
      screen.getByRole('button', { name: 'Save Changes' })
    )
    expect(
      await screen.findByText(
        'File URL lifetime must be between 1 and 168 hours'
      )
    ).toBeVisible()
    expect(fixture.put).not.toHaveBeenCalled()
    fixture.cleanup()
  })

  test('retains unsaved values when the server rejects a configuration', async () => {
    const fixture = await renderStorage()
    fixture.put.mockResolvedValue({
      data: { success: false, message: 'S3 bucket syntax is invalid' },
    })
    await fixture.user.type(
      screen.getByLabelText('S3 secret key'),
      'retry-secret'
    )
    await fixture.user.click(
      screen.getByRole('button', { name: 'Save Changes' })
    )
    await waitFor(() => expect(fixture.put).toHaveBeenCalledTimes(1))
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Save Changes' })).toBeEnabled()
    )
    expect(screen.getByLabelText('S3 secret key')).toHaveValue('retry-secret')
    expect(screen.getByRole('button', { name: 'Reset' })).toBeEnabled()
    fixture.cleanup()
  })
})
