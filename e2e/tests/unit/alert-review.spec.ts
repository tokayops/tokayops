import { test, expect } from '@playwright/test';

/**
 * The alert group a person reads after an incident: firing first, and a
 * dashboard or a runbook they can open.
 *
 * The page loads the same module app.js imports, then renders through
 * Components.alertGroupDetail. A check of the helper alone would not see
 * the links, the strip, or the escaping of the address.
 */

test.describe('alert review', () => {
  test.beforeEach(async ({ page }) => {
    await page.route('**/__alert-review-harness', route =>
      route.fulfill({
        contentType: 'text/html',
        body: '<!doctype html><script src="/js/components.js"></script>'
          + '<script type="module" src="/js/core/alert-review.js"></script>',
      }));
    await page.goto('/__alert-review-harness');
    await page.waitForFunction(() => window.AlertReview && window.Components);
  });

  test('alerts are read firing, then stale, then resolved', async ({ page }) => {
    const result = await page.evaluate(() => {
      const later = {
        status: 'firing',
        startsAt: '2026-01-01T00:00:00.0009Z',
        fingerprint: 'z',
        labels: { alertname: 'Later', instance: 'later' },
      };
      const earlier = {
        status: 'firing',
        startsAt: '2026-01-01T00:00:00.0001Z',
        fingerprint: 'a',
        labels: { alertname: 'Earlier', instance: 'earlier' },
      };
      const stale = {
        status: 'firing',
        staleSince: '2026-01-01T00:00:00Z',
        labels: { alertname: 'Stale', instance: 'stale' },
      };
      const resolved = {
        status: 'resolved',
        startsAt: '2020-01-01T00:00:00Z',
        labels: { alertname: 'Resolved', instance: 'resolved' },
      };
      const alerts = [resolved, later, stale, earlier];
      const before = alerts.slice();
      const html = window.Components.alertGroupDetail({
        id: 'ag-1', status: 'triggered', severity: 'critical',
        created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
        alerts,
      });
      const doc = new DOMParser().parseFromString(html, 'text/html');
      return {
        same: alerts.length === before.length && alerts.every((alert, i) => alert === before[i]),
        names: [...doc.querySelectorAll('.alert-item .alert-name')].map(el => el.textContent),
      };
    });

    expect(result.same).toBe(true);
    expect(result.names).toEqual([
      'instance later',
      'instance earlier',
      'instance stale',
      'instance resolved',
    ]);
  });

  test('a dashboard and a runbook are linked from the alert', async ({ page }) => {
    const html = await page.evaluate(() => window.Components.alertGroupDetail({
      id: 'ag-1', status: 'triggered', severity: 'critical',
      created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
      alerts: [{
        status: 'firing',
        labels: {
          alertname: 'Disk', instance: 'db-1',
          dashboard: 'https://grafana.example/from-label',
        },
        annotations: {
          dashboard_url: 'https://grafana.example/d/disk',
          runbook_url: 'https://runbooks.example/disk',
        },
      }],
    }));
    const doc = await parse(page, html);
    const row = doc.row;
    expect(row).toEqual([
      ['Dashboard', 'https://grafana.example/d/disk'],
      ['Runbook', 'https://runbooks.example/disk'],
    ]);
  });

  test('an empty annotation falls through and an unusable one does not', async ({ page }) => {
    const got = await page.evaluate(() => {
      const detail = (alert) => window.Components.alertGroupDetail({
        id: 'ag-1', status: 'triggered', severity: 'critical',
        created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
        alerts: [alert],
      });
      const base = { status: 'firing', labels: { alertname: 'Disk', instance: 'db-1' } };
      return {
        label: detail({
          ...base,
          labels: { ...base.labels, dashboard: 'https://grafana.example/from-label' },
          annotations: { dashboard: '' },
        }),
        blocked: detail({
          ...base,
          labels: { ...base.labels, dashboard: 'https://grafana.example/safe' },
          annotations: { dashboard: 'javascript:alert(1)' },
        }),
        blank: detail({
          ...base,
          labels: { ...base.labels, dashboard: 'https://grafana.example/safe' },
          annotations: { dashboard: '   ' },
        }),
        annotation: detail({
          ...base,
          labels: { ...base.labels, dashboard: 'https://grafana.example/from-label' },
          annotations: { dashboard: 'https://grafana.example/from-annotation' },
        }),
      };
    });

    expect(got.label).toContain('href="https://grafana.example/from-label"');
    expect(got.blocked).not.toContain('<a');
    expect(got.blank).not.toContain('<a');
    expect(got.annotation).toContain('href="https://grafana.example/from-annotation"');
    expect(got.annotation).not.toContain('href="https://grafana.example/from-label"');
  });

  test('an address the browser would rewrite is not a link', async ({ page }) => {
    const refused = await page.evaluate(() => {
      const samples = [
        'https://a b.example',
        'https://a\nb.example',
        'https://a<b.example',
        'https://a>b.example',
        'https://a|b.example',
        '/ops',
        '//evil.example',
        'javascript:alert(1)',
        'https:example.com',
        'https://',
        'https:///example.com',
        'https:\\\\example.com',
        'https://example.com\\path',
        'https://evil.example\\@good.example',
      ];
      return samples.filter(raw => {
        const html = window.Components.alertGroupDetail({
          id: 'ag-1', status: 'triggered', severity: 'critical',
          created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
          alerts: [{
            status: 'firing',
            labels: { alertname: 'Disk', instance: 'db-1' },
            annotations: { dashboard: raw },
          }],
        });
        return html.includes('<a');
      });
    });
    expect(refused).toEqual([]);
  });

  test('a quote and an ampersand in a real query are escaped', async ({ page }) => {
    const html = await page.evaluate(() => window.Components.alertGroupDetail({
      id: 'ag-1', status: 'triggered', severity: 'critical',
      created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
      alerts: [{
        status: 'firing',
        labels: { alertname: 'Disk', instance: 'db-1' },
        annotations: { dashboard: 'https://grafana.example/d/a?var-host="db"&from=now' },
      }],
    }));
    const escaped = 'https://grafana.example/d/a?var-host=&quot;db&quot;&amp;from=now';
    expect(html).toContain(`href="${escaped}"`);
    expect(html).toContain(`title="${escaped}"`);
    expect(html).not.toContain('var-host="db"');
    expect(html).not.toContain('alert-link-strip');
  });
});

async function parse(page: import('@playwright/test').Page, html: string) {
  return page.evaluate(source => {
    const doc = new DOMParser().parseFromString(source, 'text/html');
    const row = [...doc.querySelectorAll('.alert-header-marks a')].map(link => [
      link.textContent,
      link.getAttribute('href'),
    ]);
    return { row };
  }, html);
}
