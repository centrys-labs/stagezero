import { randomBytes } from 'node:crypto';
import { open, readFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';

const root = resolve(import.meta.dir, '..');

function run(command: string[], cwd = root, env = process.env): string {
	const result = Bun.spawnSync(command, { cwd, env, stdout: 'pipe', stderr: 'pipe' });
	if (result.exitCode !== 0) {
		throw new Error(`${command.slice(0, 2).join(' ')} failed: ${result.stderr.toString().trim() || result.stdout.toString().trim()}`);
	}
	return result.stdout.toString().trim();
}

function version(command: string[], minimum: number[], guide: string): void {
	let output: string;
	try {
		output = run(command);
	} catch {
		throw new Error(`${command[0]} is required. Install it from ${guide}`);
	}
	const found = output.match(/\d+\.\d+(?:\.\d+)?/)?.[0].split('.').map(Number);
	if (!found || minimum.some((n, i) => n > (found[i] ?? 0) && minimum.slice(0, i).every((v, j) => v === found[j]))) {
		throw new Error(`${command[0]} ${minimum.join('.')}+ is required (found: ${output}). See ${guide}`);
	}
}

export function assertLocalDatabaseURL(value: string): void {
	let url: URL;
	try {
		url = new URL(value);
	} catch {
		throw new Error('DATABASE_URL is invalid; use the local Compose URL from api/.env.example.');
	}
	if (
		!['postgres:', 'postgresql:'].includes(url.protocol) ||
		!['localhost', '127.0.0.1'].includes(url.hostname) ||
		url.port !== '5432' ||
		decodeURIComponent(url.username) !== 'stagezero' ||
		decodeURIComponent(url.password) !== 'password' ||
		url.pathname !== '/stagezero' ||
		url.search || url.hash
	) {
		throw new Error('DATABASE_URL is not the local StageZero Compose database. Refusing to run migrations; inspect api/.env and DATABASE_URL.');
	}
}

function envValue(text: string, name: string): string | undefined {
	const values = [...text.matchAll(new RegExp(`^${name}=(.*)$`, 'gm'))];
	if (values.length > 1) throw new Error(`Duplicate ${name} in api/.env; resolve it before setup.`);
	return values[0]?.[1].trim().replace(/^(['"])(.*)\1$/, '$2');
}

export async function createEnvironment(directory: string): Promise<void> {
	for (const folder of ['api', 'frontend']) {
		const target = join(directory, folder, '.env');
		if (await Bun.file(target).exists()) continue;
		let template = await readFile(join(directory, folder, '.env.example'), 'utf8');
		if (folder === 'api') template = template.replace(/^ENCRYPTION_KEY=$/m, `ENCRYPTION_KEY=${randomBytes(32).toString('hex')}`);
		const file = await open(target, 'wx', 0o600);
		try {
			await file.writeFile(template);
		} finally {
			await file.close();
		}
		console.log(`Created ${folder}/.env from template`);
	}
}

export async function localDatabaseEnvironment(directory = root, environment = process.env): Promise<Record<string, string | undefined>> {
	const text = await readFile(join(directory, 'api/.env'), 'utf8');
	const fileURL = envValue(text, 'DATABASE_URL');
	if (!fileURL) throw new Error('Set DATABASE_URL in api/.env to the local Compose URL before migrating.');
	assertLocalDatabaseURL(fileURL);
	const selectedURL = environment.DATABASE_URL || fileURL;
	assertLocalDatabaseURL(selectedURL);
	const key = environment.ENCRYPTION_KEY || envValue(text, 'ENCRYPTION_KEY');
	if (!key || !/^[0-9a-fA-F]{64}$/.test(key)) {
		throw new Error('Set a valid 64-character hex ENCRYPTION_KEY in api/.env; existing keys are never replaced.');
	}
	return { ...environment, DATABASE_URL: selectedURL, ENCRYPTION_KEY: key };
}

function checkDocker(): void {
	version(['docker', '--version'], [24, 0], 'https://docs.docker.com/get-docker/');
	version(['docker', 'compose', 'version'], [2, 0], 'https://docs.docker.com/compose/install/');
	const host = process.env.DOCKER_HOST || run(['docker', 'context', 'inspect', '--format', '{{.Endpoints.docker.Host}}']);
	if (!host.startsWith('unix://') && !host.startsWith('npipe://')) {
		throw new Error('Docker is not using a local socket; switch to a local Docker context before setup or migrations.');
	}
	try {
		run(['docker', 'info', '--format', '{{.ServerVersion}}']);
	} catch {
		throw new Error('Docker daemon is unavailable. Start Docker and ensure your user has Docker access.');
	}
}

function migrate(env: Record<string, string | undefined>): void {
	const id = run(['docker', 'compose', 'ps', '-q', 'postgres']);
	if (!id || run(['docker', 'inspect', '--format', '{{.State.Health.Status}}', id]) !== 'healthy') {
		throw new Error('Local Compose postgres is not healthy. Run bun run db:up and check docker compose logs postgres.');
	}
	const published = run(['docker', 'compose', 'port', 'postgres', '5432']);
	if (!published.endsWith(':5432')) {
		throw new Error('Compose postgres is not published on local port 5432; migrations were not run.');
	}
	try {
		if (run(['docker', 'compose', 'exec', '-T', '-e', 'PGPASSWORD=password', 'postgres', 'psql', '-h', 'localhost', '-U', 'stagezero', '-d', 'stagezero', '-tAc', 'SELECT current_database()']) !== 'stagezero') {
			throw new Error('Database identity mismatch');
		}
	} catch {
		throw new Error('Compose postgres does not accept the expected local credentials. Check for an existing volume initialized with different users; no data was changed.');
	}
	console.log('Applying migrations to the verified local Compose database');
	run(['go', 'run', './cmd/migrate'], join(root, 'api'), env);
	console.log('Migrations complete');
}

export async function main(args: string[]): Promise<void> {
	if (args.includes('--help')) {
		console.log('Usage: bun run setup [--skip-db] [--skip-install] | bun run db:migrate');
		return;
	}
	if (args.some((arg) => !['--skip-db', '--skip-install', '--migrate'].includes(arg)) || (args.includes('--migrate') && args.length !== 1)) {
		throw new Error('Unknown or incompatible option. Run bun run setup --help.');
	}
	const onlyMigrate = args.includes('--migrate');
	version(['go', 'version'], [1, 25], 'https://go.dev/dl/');
	if (!onlyMigrate) {
		version(['node', '--version'], [22, 0], 'https://nodejs.org/');
		version(['bun', '--version'], [1, 4, 3], 'https://bun.sh/docs/installation');
	}
	if (!args.includes('--skip-db')) checkDocker();
	if (!onlyMigrate) {
		await createEnvironment(root);
		if (!args.includes('--skip-install')) {
			console.log('Installing locked Bun workspace and Go modules');
			run(['bun', 'install', '--frozen-lockfile']);
			run(['go', 'mod', 'download'], join(root, 'api'));
		}
	}
	if (!args.includes('--skip-db')) {
		const env = await localDatabaseEnvironment();
		if (!onlyMigrate) {
			console.log('Starting local Compose postgres');
			run(['docker', 'compose', 'up', '-d', '--wait', 'postgres']);
		}
		migrate(env);
	}
	if (!onlyMigrate) console.log('Setup complete. Run bun run dev. OAuth features require provider credentials in api/.env.');
}

if (import.meta.main) {
	main(process.argv.slice(2)).catch((error) => {
		console.error(`Setup failed: ${error.message}`);
		process.exitCode = 1;
	});
}
