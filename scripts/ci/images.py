#!/usr/bin/env python3
"""Transfer the exact tested single-platform images; publication never rebuilds."""
import argparse
import fnmatch
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parents[2]
IMAGES = json.loads((ROOT / '.github/ci-images.json').read_text())
PRODUCTS = json.loads((ROOT / '.github/ci-products.json').read_text())


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def require(condition, message):
    if not condition:
        raise ValueError(message)


def revision():
    value = os.environ['GITHUB_SHA']
    require(re.fullmatch(r'[0-9a-f]{40}', value), 'invalid source revision')
    return value


def repository(component):
    return 'ghcr.io/' + os.environ['GITHUB_REPOSITORY_OWNER'].lower() + '/' + IMAGES[component]['repository']


def local_image(component):
    return IMAGES[component]['local_tag'] + ':' + revision()


def inspect(ref):
    return json.loads(command('docker', 'image', 'inspect', ref))[0]


def verify_identity(info, architecture):
    require(info['Architecture'] == architecture, 'image architecture differs')
    labels = info['Config'].get('Labels') or {}
    require(labels.get('org.opencontainers.image.revision') == revision(), 'image source revision differs')
    require(labels.get('org.opencontainers.image.source') == 'https://github.com/' + os.environ['GITHUB_REPOSITORY'], 'image source repository differs')


def matrix(product, publish=False):
    return {'include': [dict(component=name, arch=arch,
                            runner='ubuntu-24.04-arm' if arch == 'arm64' else 'ubuntu-24.04', **value)
                        for name, value in IMAGES.items() if value['product'] == product and (not publish or value['publish'])
                        for arch in value['architectures']]}


def checksum(path):
    with path.open('rb') as file:
        return hashlib.file_digest(file, 'sha256').hexdigest()


def export_image(component, architecture, directory):
    directory.mkdir(parents=True, exist_ok=True)
    ref = local_image(component)
    info = inspect(ref)
    verify_identity(info, architecture)
    archive = directory / 'image.tar'
    subprocess.run(['docker', 'save', '--output', str(archive), ref], check=True)
    metadata = {'component': component, 'architecture': architecture, 'commit': revision(),
                'image_id': info['Id'], 'archive_sha256': checksum(archive)}
    (directory / 'image.json').write_text(json.dumps(metadata, indent=2) + '\n')


def load_image(component, architecture, directory):
    metadata = json.loads((directory / 'image.json').read_text())
    require(metadata['component'] == component and metadata['architecture'] == architecture, 'wrong image artifact')
    require(metadata['commit'] == revision(), 'artifact belongs to another commit')
    archive = directory / 'image.tar'
    require(checksum(archive) == metadata['archive_sha256'], 'image archive checksum differs')
    subprocess.run(['docker', 'load', '--input', str(archive)], check=True)
    info = inspect(local_image(component))
    verify_identity(info, architecture)
    require(info['Id'] == metadata['image_id'], 'loaded image differs from tested image')
    return info['Id']


def push_image(component, architecture, directory):
    image_id = load_image(component, architecture, directory)
    repo = repository(component)
    tag = repo + ':' + IMAGES[component]['tag_prefix'] + revision() + '-' + architecture
    subprocess.run(['docker', 'tag', image_id, tag], check=True)
    subprocess.run(['docker', 'push', tag], check=True)
    digests = [value for value in inspect(tag)['RepoDigests'] if value.startswith(repo + '@sha256:')]
    require(len(digests) == 1, 'cannot resolve pushed image digest')
    result = {'component': component, 'architecture': architecture, 'commit': revision(), 'ref': digests[0]}
    (directory / f'digest-{component}-{architecture}.json').write_text(json.dumps(result) + '\n')


def affects_product(product, path):
    config = PRODUCTS[product]
    included = 'include' not in config or any(fnmatch.fnmatchcase(path, pattern) for pattern in config['include'])
    return included and not any(fnmatch.fnmatchcase(path, pattern) for pattern in config.get('exclude', []))


def current_product(product):
    if os.environ['GITHUB_REF'] != 'refs/heads/main':
        return True
    remote = command('git', 'ls-remote', '--exit-code', 'origin', 'refs/heads/main').split()[0]
    require(re.fullmatch(r'[0-9a-f]{40}', remote), 'invalid remote main revision')
    if remote == revision():
        return True
    subprocess.run(['git', 'fetch', '--no-tags', '--depth=1', 'origin', remote], check=True)
    changed = command('git', 'diff', '--name-only', '-z', revision(), remote, '--').split('\0')
    # An unrelated product commit must not strand this product's verified release.
    return not any(affects_product(product, path) for path in changed if path)


def version_tags(ref):
    match = re.fullmatch(r'refs/tags/v(\d+)\.(\d+)\.(\d+)(-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?', ref)
    if not match:
        return []
    tags = [ref.removeprefix('refs/tags/v').replace('+', '_')]
    if not match[4]:
        tags.append(match[1] + '.' + match[2])
    return tags


def release(product, directory):
    records = [json.loads(p.read_text()) for p in directory.glob('digest-*.json')]
    components = {name: value for name, value in IMAGES.items() if value['product'] == product and value['publish']}
    expected = {(name, arch) for name, value in components.items() for arch in value['architectures']}
    require(len(records) == len(expected) and {(r['component'], r['architecture']) for r in records} == expected,
            'incomplete or duplicate platform digests')
    for record in records:
        require(record['commit'] == revision(), 'digest belongs to another commit')
        require(re.fullmatch(re.escape(repository(record['component'])) + r'@sha256:[0-9a-f]{64}', record['ref']), 'invalid registry digest')
    manifest = {'commit': revision(), 'product': product}
    references = {}
    # Create every immutable component index before advancing any mutable tag.
    for name, value in components.items():
        repo = repository(name)
        tag = repo + ':' + value['tag_prefix'] + revision()
        sources = [r['ref'] for r in records if r['component'] == name]
        subprocess.run(['docker', 'buildx', 'imagetools', 'create', '--tag', tag, *sources], check=True)
        digest = command('docker', 'buildx', 'imagetools', 'inspect', '--format', '{{.Manifest.Digest}}', tag)
        require(re.fullmatch(r'sha256:[0-9a-f]{64}', digest), 'invalid multi-platform digest')
        references[name] = repo + '@' + digest
        manifest[name.replace('-', '_') + '_image'] = references[name]
    fresh = current_product(product)
    for name, value in components.items():
        tags = []
        if name == 'main':
            tags.append('sha-' + revision()[:7])
        if name != 'edge':
            tags += version_tags(os.environ['GITHUB_REF'])
            if fresh and os.environ['GITHUB_REF'] == 'refs/heads/main':
                tags += ['main', 'latest'] if name == 'main' else ['latest']
        if tags:
            args = ['docker', 'buildx', 'imagetools', 'create']
            for tag in tags:
                args += ['--tag', repository(name) + ':' + tag]
            subprocess.run([*args, references[name]], check=True)
    manifest['promoted'] = fresh
    if not fresh:
        print('A newer change affects this product; immutable images retained, mutable tags unchanged.')
    (directory / 'release-images.json').write_text(json.dumps(manifest, indent=2) + '\n')
    with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as summary:
        summary.write('Verified images (published without rebuilding):\n\n```json\n' + json.dumps(manifest, indent=2) + '\n```\n')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    command_parser = commands.add_parser('matrix')
    command_parser.add_argument('product', choices=PRODUCTS)
    command_parser.add_argument('--publish', action='store_true')
    for name in ('export', 'load', 'push'):
        command_parser = commands.add_parser(name)
        command_parser.add_argument('component', choices=IMAGES)
        command_parser.add_argument('architecture', choices=('amd64', 'arm64'))
        command_parser.add_argument('directory', type=Path)
    command_parser = commands.add_parser('release')
    command_parser.add_argument('product', choices=PRODUCTS)
    command_parser.add_argument('directory', type=Path)
    args = parser.parse_args()
    if args.command == 'matrix':
        print(json.dumps(matrix(args.product, args.publish), separators=(',', ':')))
    elif args.command == 'release':
        release(args.product, args.directory)
    else:
        {'export': export_image, 'load': load_image, 'push': push_image}[args.command](args.component, args.architecture, args.directory)


if __name__ == '__main__':
    main()
