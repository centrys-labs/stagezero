import { afterEach, expect, test } from 'bun:test';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { assertLocalDatabaseURL, createEnvironment, localDatabaseEnvironment } from './setup';

const folders: string[] = [];
afterEach(async () => {
	for (const path of folders.splice(0)) await rm(path, { recursive: true, force: true });
});

test('rejects remote, mismatched, and malformed database URLs', () => {
	for (const url of [
		'postgres://stagezero:password@db.example.com:5432/stagezero',
		'postgres://stagezero:password@localhost:5433/stagezero',
		'postgres://stagezero:password@localhost:5432/production',
		'postgres://other:password@localhost:5432/stagezero',
		'postgres://stagezero:password@localhost:5432/stagezero?host=remote',
		'not a URL'
	]) expect(() => assertLocalDatabaseURL(url)).toThrow();
	expect(() => assertLocalDatabaseURL('postgres://stagezero:password@localhost:5432/stagezero')).not.toThrow();
});

test('creates private env files, generates a persistent key, and preserves user config', async () => {
	const path = await mkdtemp(join(tmpdir(), 'stagezero-setup-'));
	folders.push(path);
	await mkdir(join(path, 'api'));
	await mkdir(join(path, 'frontend'));
	await writeFile(join(path, 'api/.env.example'), 'DATABASE_URL=postgres://stagezero:password@localhost:5432/stagezero\nENCRYPTION_KEY=\n');
	await writeFile(join(path, 'frontend/.env.example'), 'API_BASE_URL=http://localhost:8080\n');
	await createEnvironment(path);
	const api = await readFile(join(path, 'api/.env'), 'utf8');
	expect(api).toMatch(/ENCRYPTION_KEY=[a-f0-9]{64}\n/);
	expect((await Bun.file(join(path, 'api/.env')).stat()).mode & 0o777).toBe(0o600);
	await writeFile(join(path, 'frontend/.env'), 'CUSTOM=value\n');
	await createEnvironment(path);
	expect(await readFile(join(path, 'api/.env'), 'utf8')).toBe(api);
	expect(await readFile(join(path, 'frontend/.env'), 'utf8')).toBe('CUSTOM=value\n');
	expect((await localDatabaseEnvironment(path, {})).ENCRYPTION_KEY).toMatch(/^[a-f0-9]{64}$/);
	expect(() => assertLocalDatabaseURL('postgres://stagezero:password@127.0.0.1:5432/stagezero')).not.toThrow();
	await expect(localDatabaseEnvironment(path, { DATABASE_URL: 'postgres://stagezero:password@db.example.com:5432/stagezero' })).rejects.toThrow('Refusing to run migrations');
	await writeFile(join(path, 'api/.env'), 'DATABASE_URL=postgres://stagezero:password@db.example.com:5432/stagezero\nENCRYPTION_KEY=abc\n');
	await expect(localDatabaseEnvironment(path, { DATABASE_URL: 'postgres://stagezero:password@localhost:5432/stagezero' })).rejects.toThrow('Refusing to run migrations');
});

test('missing Go stops before writing any env files', async () => {
	const result = Bun.spawnSync([process.execPath, 'scripts/setup.ts', '--skip-db'], {
		cwd: join(import.meta.dir, '..'),
		env: { PATH: '/nonexistent' },
		stdout: 'pipe',
		stderr: 'pipe'
	});
	expect(result.exitCode).toBe(1);
	expect(result.stderr.toString()).toContain('go is required');
});
